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

package tagging_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/samber/lo"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1beta1 "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	"github.com/aws/karpenter-provider-aws/pkg/apis"
	"github.com/aws/karpenter-provider-aws/pkg/apis/v1beta1"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/nodeclaim/tagging"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instance"
	"github.com/aws/karpenter-provider-aws/pkg/test"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var awsEnv *test.Environment
var env *coretest.Environment
var taggingController *tagging.Controller

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "TaggingController")
}

var _ = BeforeSuite(func() {
	env = coretest.NewEnvironment(scheme.Scheme, coretest.WithCRDs(apis.CRDs...))
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx = options.ToContext(ctx, test.Options())
	awsEnv = test.NewEnvironment(ctx, env)
	taggingController = tagging.NewController(env.Client, awsEnv.InstanceProvider)
})
var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	awsEnv.Reset()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("TaggingController", func() {
	var ec2Instance *ec2.Instance

	BeforeEach(func() {
		ec2Instance = &ec2.Instance{
			State: &ec2.InstanceState{
				Name: aws.String(ec2.InstanceStateNameRunning),
			},
			Tags: []*ec2.Tag{
				{
					Key:   aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", options.FromContext(ctx).ClusterName)),
					Value: aws.String("owned"),
				},
				{
					Key:   aws.String(corev1beta1.NodePoolLabelKey),
					Value: aws.String("default"),
				},
				{
					Key:   aws.String(corev1beta1.ManagedByAnnotationKey),
					Value: aws.String(options.FromContext(ctx).ClusterName),
				},
			},
			PrivateDnsName: aws.String(fake.PrivateDNSName()),
			Placement: &ec2.Placement{
				AvailabilityZone: aws.String(fake.DefaultRegion),
			},
			InstanceId:   aws.String(fake.InstanceID()),
			InstanceType: aws.String("m5.large"),
		}

		awsEnv.EC2API.Instances.Store(*ec2Instance.InstanceId, ec2Instance)
	})

	It("shouldn't tag instances without a Node", func() {
		nodeClaim := coretest.NodeClaim(corev1beta1.NodeClaim{
			Status: corev1beta1.NodeClaimStatus{
				ProviderID: fake.ProviderID(*ec2Instance.InstanceId),
			},
		})

		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, taggingController, nodeClaim)
		Expect(nodeClaim.Annotations).To(Not(HaveKey(v1beta1.AnnotationInstanceTagged)))
		Expect(lo.ContainsBy(ec2Instance.Tags, func(tag *ec2.Tag) bool {
			return *tag.Key == v1beta1.TagName
		})).To(BeFalse())
	})

	It("shouldn't tag nodeclaim with a malformed provderID", func() {
		nodeClaim := coretest.NodeClaim(corev1beta1.NodeClaim{
			Status: corev1beta1.NodeClaimStatus{
				ProviderID: "Bad providerID",
				NodeName:   "default",
			},
		})

		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, taggingController, nodeClaim)
		Expect(nodeClaim.Annotations).To(Not(HaveKey(v1beta1.AnnotationInstanceTagged)))
		Expect(lo.ContainsBy(ec2Instance.Tags, func(tag *ec2.Tag) bool {
			return *tag.Key == v1beta1.TagName
		})).To(BeFalse())
	})

	It("should gracefully handle missing NodeClaim", func() {
		nodeClaim := coretest.NodeClaim(corev1beta1.NodeClaim{
			Status: corev1beta1.NodeClaimStatus{
				ProviderID: fake.ProviderID(*ec2Instance.InstanceId),
				NodeName:   "default",
			},
		})

		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectDeleted(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, taggingController, nodeClaim)
	})

	It("should gracefully handle missing instance", func() {
		nodeClaim := coretest.NodeClaim(corev1beta1.NodeClaim{
			Status: corev1beta1.NodeClaimStatus{
				ProviderID: fake.ProviderID(*ec2Instance.InstanceId),
				NodeName:   "default",
			},
		})

		ExpectApplied(ctx, env.Client, nodeClaim)
		awsEnv.EC2API.Instances.Delete(*ec2Instance.InstanceId)
		ExpectObjectReconciled(ctx, env.Client, taggingController, nodeClaim)
		Expect(nodeClaim.Annotations).To(Not(HaveKey(v1beta1.AnnotationInstanceTagged)))
	})

	It("shouldn't tag nodeclaim with deletion timestamp set", func() {
		nodeClaim := coretest.NodeClaim(corev1beta1.NodeClaim{
			Status: corev1beta1.NodeClaimStatus{
				ProviderID: fake.ProviderID(*ec2Instance.InstanceId),
				NodeName:   "default",
			},
			ObjectMeta: v1.ObjectMeta{
				Finalizers: []string{"testing/finalizer"},
			},
		})

		ExpectApplied(ctx, env.Client, nodeClaim)
		Expect(env.Client.Delete(ctx, nodeClaim)).To(Succeed())
		ExpectObjectReconciled(ctx, env.Client, taggingController, nodeClaim)
		Expect(nodeClaim.Annotations).To(Not(HaveKey(v1beta1.AnnotationInstanceTagged)))
		Expect(lo.ContainsBy(ec2Instance.Tags, func(tag *ec2.Tag) bool {
			return *tag.Key == v1beta1.TagName
		})).To(BeFalse())
	})

	DescribeTable(
		"should tag taggable instances",
		func(customTags ...string) {
			nodeClaim := coretest.NodeClaim(corev1beta1.NodeClaim{
				Status: corev1beta1.NodeClaimStatus{
					ProviderID: fake.ProviderID(*ec2Instance.InstanceId),
					NodeName:   "default",
				},
			})

			for _, tag := range customTags {
				ec2Instance.Tags = append(ec2Instance.Tags, &ec2.Tag{
					Key:   aws.String(tag),
					Value: aws.String("custom-tag"),
				})
			}
			awsEnv.EC2API.Instances.Store(*ec2Instance.InstanceId, ec2Instance)

			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, taggingController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKey(v1beta1.AnnotationInstanceTagged))

			expectedTags := map[string]string{
				v1beta1.TagName:      nodeClaim.Status.NodeName,
				v1beta1.TagNodeClaim: nodeClaim.Name,
			}
			instanceTags := instance.NewInstance(ec2Instance).Tags
			for tag, value := range expectedTags {
				if lo.Contains(customTags, tag) {
					value = "custom-tag"
				}
				Expect(instanceTags).To(HaveKeyWithValue(tag, value))
			}
		},
		Entry("with only karpenter.k8s.aws/nodeclaim tag", v1beta1.TagName),
		Entry("with only Name tag", v1beta1.TagNodeClaim),
		Entry("with both Name and karpenter.k8s.aws/nodeclaim tags"),
		Entry("with nothing to tag", v1beta1.TagName, v1beta1.TagNodeClaim),
	)
})
