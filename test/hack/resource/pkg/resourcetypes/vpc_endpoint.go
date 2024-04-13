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

package resourcetypes

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
	"golang.org/x/exp/slices"
)

type VPCEndpoint struct {
	ec2Client *ec2.Client
}

func NewVPCEndpoint(ec2Client *ec2.Client) *VPCEndpoint {
	return &VPCEndpoint{ec2Client: ec2Client}
}

func (v *VPCEndpoint) String() string {
	return "VPCEndpoints"
}

func (v *VPCEndpoint) Global() bool {
	return false
}

func (v *VPCEndpoint) Get(ctx context.Context, clusterName string) (ids []string, err error) {
	var nextToken *string
	for {
		out, err := v.ec2Client.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{
			Filters: []ec2types.Filter{
				{
					Name:   lo.ToPtr("tag:" + karpenterTestingTag),
					Values: []string{clusterName},
				},
			},
			NextToken: nextToken,
		})
		if err != nil {
			return ids, err
		}
		for _, endpoint := range out.VpcEndpoints {
			ids = append(ids, lo.FromPtr(endpoint.VpcEndpointId))
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return ids, err
}

func (v *VPCEndpoint) CountAll(ctx context.Context) (count int, err error) {
	var nextToken *string
	for {
		out, err := v.ec2Client.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return count, err
		}

		count += len(out.VpcEndpoints)

		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return count, err
}

func (v *VPCEndpoint) GetExpired(ctx context.Context, expirationTime time.Time, excludedClusters []string) (ids []string, err error) {
	var nextToken *string
	for {
		out, err := v.ec2Client.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{
			Filters: []ec2types.Filter{
				{
					Name:   lo.ToPtr("tag-key"),
					Values: []string{karpenterTestingTag},
				},
			},
			NextToken: nextToken,
		})
		if err != nil {
			return ids, err
		}
		for _, endpoint := range out.VpcEndpoints {
			clusterName, found := lo.Find(endpoint.Tags, func(tag ec2types.Tag) bool {
				return *tag.Key == k8sClusterTag
			})
			if found && slices.Contains(excludedClusters, lo.FromPtr(clusterName.Value)) {
				continue
			}
			if endpoint.CreationTimestamp.Before(expirationTime) {
				ids = append(ids, lo.FromPtr(endpoint.VpcEndpointId))
			}
		}
		nextToken = out.NextToken
		if nextToken == nil {
			break
		}
	}
	return ids, err
}

// Cleanup any old VPC endpoints that were provisioned as part of testing
func (v *VPCEndpoint) Cleanup(ctx context.Context, ids []string) ([]string, error) {
	if _, err := v.ec2Client.DeleteVpcEndpoints(ctx, &ec2.DeleteVpcEndpointsInput{
		VpcEndpointIds: ids,
	}); err != nil {
		return nil, err
	}
	return ids, nil
}
