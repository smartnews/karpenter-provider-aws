/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package interruption

import (
	"context"
	"fmt"
	"time"

	sqsapi "github.com/aws/aws-sdk-go/service/sqs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"github.com/aws/karpenter-provider-aws/pkg/cache"
	interruptionevents "github.com/aws/karpenter-provider-aws/pkg/controllers/interruption/events"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/interruption/messages"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/interruption/messages/statechange"
	"github.com/aws/karpenter-provider-aws/pkg/providers/sqs"
	"github.com/aws/karpenter-provider-aws/pkg/utils"

	"sigs.k8s.io/karpenter/pkg/events"
	corecontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
)

type Action string

const (
	CordonAndDrain Action = "CordonAndDrain"
	NoAction       Action = "NoAction"
)

// Controller is an AWS interruption controller.
// It continually polls an SQS queue for events from aws.ec2 and aws.health that
// trigger node health events or node spot interruption/rebalance events.
type Controller struct {
	kubeClient                client.Client
	clk                       clock.Clock
	recorder                  events.Recorder
	sqsProvider               sqs.Provider
	unavailableOfferingsCache *cache.UnavailableOfferings
	parser                    *EventParser
	cm                        *pretty.ChangeMonitor
}

func NewController(kubeClient client.Client, clk clock.Clock, recorder events.Recorder,
	sqsProvider sqs.Provider, unavailableOfferingsCache *cache.UnavailableOfferings) *Controller {

	return &Controller{
		kubeClient:                kubeClient,
		clk:                       clk,
		recorder:                  recorder,
		sqsProvider:               sqsProvider,
		unavailableOfferingsCache: unavailableOfferingsCache,
		parser:                    NewEventParser(DefaultParsers...),
		cm:                        pretty.NewChangeMonitor(),
	}
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("queue", c.sqsProvider.Name()))
	if c.cm.HasChanged(c.sqsProvider.Name(), nil) {
		logging.FromContext(ctx).Debugf("watching interruption queue")
	}
	sqsMessages, err := c.sqsProvider.GetSQSMessages(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting messages from queue, %w", err)
	}
	if len(sqsMessages) == 0 {
		return reconcile.Result{}, nil
	}
	nodeClaimInstanceIDMap, err := c.makeNodeClaimInstanceIDMap(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("making nodeclaim instance id map, %w", err)
	}
	nodeInstanceIDMap, err := c.makeNodeInstanceIDMap(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("making node instance id map, %w", err)
	}
	errs := make([]error, len(sqsMessages))
	workqueue.ParallelizeUntil(ctx, 10, len(sqsMessages), func(i int) {
		msg, e := c.parseMessage(sqsMessages[i])
		if e != nil {
			// If we fail to parse, then we should delete the message but still log the error
			logging.FromContext(ctx).Errorf("parsing message, %v", e)
			errs[i] = c.deleteMessage(ctx, sqsMessages[i])
			return
		}
		if e = c.handleMessage(ctx, nodeClaimInstanceIDMap, nodeInstanceIDMap, msg); e != nil {
			errs[i] = fmt.Errorf("handling message, %w", e)
			return
		}
		errs[i] = c.deleteMessage(ctx, sqsMessages[i])
	})
	if err = multierr.Combine(errs...); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Name() string {
	return "interruption"
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.NewSingletonManagedBy(m)
}

// parseMessage parses the passed SQS message into an internal Message interface
func (c *Controller) parseMessage(raw *sqsapi.Message) (messages.Message, error) {
	// No message to parse in this case
	if raw == nil || raw.Body == nil {
		return nil, fmt.Errorf("message or message body is nil")
	}
	msg, err := c.parser.Parse(*raw.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing sqs message, %w", err)
	}
	return msg, nil
}

// handleMessage takes an action against every node involved in the message that is owned by a NodePool
func (c *Controller) handleMessage(ctx context.Context, nodeClaimInstanceIDMap map[string]*v1beta1.NodeClaim,
	nodeInstanceIDMap map[string]*v1.Node, msg messages.Message) (err error) {

	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("messageKind", msg.Kind()))
	receivedMessages.WithLabelValues(string(msg.Kind())).Inc()

	if msg.Kind() == messages.NoOpKind {
		return nil
	}
	for _, instanceID := range msg.EC2InstanceIDs() {
		nodeClaim, ok := nodeClaimInstanceIDMap[instanceID]
		if !ok {
			continue
		}
		node := nodeInstanceIDMap[instanceID]
		if e := c.handleNodeClaim(ctx, msg, nodeClaim, node); e != nil {
			err = multierr.Append(err, e)
		}
	}
	messageLatency.Observe(time.Since(msg.StartTime()).Seconds())
	if err != nil {
		return fmt.Errorf("acting on NodeClaims, %w", err)
	}
	return nil
}

// deleteMessage removes the passed SQS message from the queue and fires a metric for the deletion
func (c *Controller) deleteMessage(ctx context.Context, msg *sqsapi.Message) error {
	if err := c.sqsProvider.DeleteSQSMessage(ctx, msg); err != nil {
		return fmt.Errorf("deleting sqs message, %w", err)
	}
	deletedMessages.Inc()
	return nil
}

// handleNodeClaim retrieves the action for the message and then performs the appropriate action against the node
func (c *Controller) handleNodeClaim(ctx context.Context, msg messages.Message, nodeClaim *v1beta1.NodeClaim, node *v1.Node) error {
	action := actionForMessage(msg)
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("nodeclaim", nodeClaim.Name, "action", string(action)))
	if node != nil {
		ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))
	}

	// Record metric and event for this action
	c.notifyForMessage(msg, nodeClaim, node)
	actionsPerformed.WithLabelValues(string(action)).Inc()

	// Mark the offering as unavailable in the ICE cache since we got a spot interruption warning
	if msg.Kind() == messages.SpotInterruptionKind {
		zone := nodeClaim.Labels[v1.LabelTopologyZone]
		instanceType := nodeClaim.Labels[v1.LabelInstanceTypeStable]
		if zone != "" && instanceType != "" {
			c.unavailableOfferingsCache.MarkUnavailable(ctx, string(msg.Kind()), instanceType, zone, v1beta1.CapacityTypeSpot)
		}
		// try to create a new nodeclaim immediately but ignore error if it fails
		if err := c.createNodeClaim(ctx, nodeClaim); err != nil {
			logging.FromContext(ctx).Errorf("[interruption handling]failed to create a new nodeclaim, %v", err)
		}
	}
	if action != NoAction {
		return c.deleteNodeClaim(ctx, nodeClaim, node)
	}
	return nil
}

// createNodeClaim creates a new NodeClaim with the same spec of the interrupted one
func (c *Controller) createNodeClaim(ctx context.Context, oldNodeClaim *v1beta1.NodeClaim) error {
	newNodeClaim := &v1beta1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName:    oldNodeClaim.ObjectMeta.GenerateName,
			Annotations:     oldNodeClaim.ObjectMeta.Annotations,
			Labels:          oldNodeClaim.ObjectMeta.Labels,
			OwnerReferences: oldNodeClaim.ObjectMeta.OwnerReferences,
		},
		Spec: oldNodeClaim.Spec,
	}
	return c.kubeClient.Create(ctx, newNodeClaim)
}

// deleteNodeClaim removes the NodeClaim from the api-server
func (c *Controller) deleteNodeClaim(ctx context.Context, nodeClaim *v1beta1.NodeClaim, node *v1.Node) error {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("deleting the node on interruption message, %w", err))
	}
	logging.FromContext(ctx).Infof("initiating delete from interruption message")
	c.recorder.Publish(interruptionevents.TerminatingOnInterruption(node, nodeClaim)...)
	metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:       terminationReasonLabel,
		metrics.NodePoolLabel:     nodeClaim.Labels[v1beta1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[v1beta1.CapacityTypeLabelKey],
	}).Inc()
	return nil
}

// notifyForMessage publishes the relevant alert based on the message kind
func (c *Controller) notifyForMessage(msg messages.Message, nodeClaim *v1beta1.NodeClaim, n *v1.Node) {
	switch msg.Kind() {
	case messages.RebalanceRecommendationKind:
		c.recorder.Publish(interruptionevents.RebalanceRecommendation(n, nodeClaim)...)

	case messages.ScheduledChangeKind:
		c.recorder.Publish(interruptionevents.Unhealthy(n, nodeClaim)...)

	case messages.SpotInterruptionKind:
		c.recorder.Publish(interruptionevents.SpotInterrupted(n, nodeClaim)...)

	case messages.StateChangeKind:
		typed := msg.(statechange.Message)
		if lo.Contains([]string{"stopping", "stopped"}, typed.Detail.State) {
			c.recorder.Publish(interruptionevents.Stopping(n, nodeClaim)...)
		} else {
			c.recorder.Publish(interruptionevents.Terminating(n, nodeClaim)...)
		}

	default:
	}
}

// makeNodeClaimInstanceIDMap builds a map between the instance id that is stored in the
// NodeClaim .status.providerID and the NodeClaim
func (c *Controller) makeNodeClaimInstanceIDMap(ctx context.Context) (map[string]*v1beta1.NodeClaim, error) {
	m := map[string]*v1beta1.NodeClaim{}
	nodeClaimList := &v1beta1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaimList); err != nil {
		return nil, err
	}
	for i := range nodeClaimList.Items {
		if nodeClaimList.Items[i].Status.ProviderID == "" {
			continue
		}
		id, err := utils.ParseInstanceID(nodeClaimList.Items[i].Status.ProviderID)
		if err != nil || id == "" {
			continue
		}
		m[id] = &nodeClaimList.Items[i]
	}
	return m, nil
}

// makeNodeInstanceIDMap builds a map between the instance id that is stored in the
// node .spec.providerID and the node
func (c *Controller) makeNodeInstanceIDMap(ctx context.Context) (map[string]*v1.Node, error) {
	m := map[string]*v1.Node{}
	nodeList := &v1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	for i := range nodeList.Items {
		if nodeList.Items[i].Spec.ProviderID == "" {
			continue
		}
		id, err := utils.ParseInstanceID(nodeList.Items[i].Spec.ProviderID)
		if err != nil || id == "" {
			continue
		}
		m[id] = &nodeList.Items[i]
	}
	return m, nil
}

func actionForMessage(msg messages.Message) Action {
	switch msg.Kind() {
	case messages.ScheduledChangeKind, messages.SpotInterruptionKind, messages.StateChangeKind:
		return CordonAndDrain
	default:
		return NoAction
	}
}
