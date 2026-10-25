package kubeovn_vpc_nat_gateway

import (
	"context"
	"testing"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnfake "github.com/kubeovn/kube-ovn/pkg/client/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetLanNetworkAttachment(t *testing.T) {
	const subnetName = "subnet-internal"

	testcases := []struct {
		name               string
		provider           string
		annotations        map[string]string
		expectedAttachment string // "" when the annotation must be absent
		expectError        bool
	}{
		{
			name:               "Harvester overlay network",
			provider:           "vswitchinternal.tenant-net.ovn",
			expectedAttachment: "tenant-net/vswitchinternal",
		},
		{
			name:               "keeps the other annotations",
			provider:           "vswitchinternal.tenant-net.ovn",
			annotations:        map[string]string{"description": "gw"},
			expectedAttachment: "tenant-net/vswitchinternal",
		},
		{
			name:     "default ovn provider",
			provider: kubeOVNProvider,
		},
		{
			name: "no provider",
		},
		{
			name:        "malformed provider",
			provider:    "vswitchinternal.ovn",
			expectError: true,
		},
		{
			name:        "provider of another CNI",
			provider:    "vswitchinternal.tenant-net.macvlan",
			expectError: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			subnets := kubeovnfake.NewSimpleClientset().KubeovnV1().Subnets()
			subnet := &kubeovnv1.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: subnetName},
				Spec:       kubeovnv1.SubnetSpec{Provider: tc.provider},
			}
			if _, err := subnets.Create(context.Background(), subnet, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			gateway := &kubeovnv1.VpcNatGateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Annotations: tc.annotations},
				Spec:       kubeovnv1.VpcNatGatewaySpec{Subnet: subnetName},
			}

			err := setLanNetworkAttachment(context.Background(), subnets, gateway)
			if tc.expectError {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			attachment, ok := gateway.Annotations[nadv1.NetworkAttachmentAnnot]
			if tc.expectedAttachment == "" {
				if ok {
					t.Errorf("unexpected annotation %s: %q", nadv1.NetworkAttachmentAnnot, attachment)
				}
				return
			}
			if attachment != tc.expectedAttachment {
				t.Errorf("annotation %s = %q, want %q", nadv1.NetworkAttachmentAnnot, attachment, tc.expectedAttachment)
			}
			for k, v := range tc.annotations {
				if gateway.Annotations[k] != v {
					t.Errorf("annotation %s = %q, want %q", k, gateway.Annotations[k], v)
				}
			}
		})
	}
}

func TestSetLanNetworkAttachmentMissingSubnet(t *testing.T) {
	subnets := kubeovnfake.NewSimpleClientset().KubeovnV1().Subnets()
	gateway := &kubeovnv1.VpcNatGateway{Spec: kubeovnv1.VpcNatGatewaySpec{Subnet: "missing"}}
	if err := setLanNetworkAttachment(context.Background(), subnets, gateway); err == nil {
		t.Fatal("expected an error")
	}
}
