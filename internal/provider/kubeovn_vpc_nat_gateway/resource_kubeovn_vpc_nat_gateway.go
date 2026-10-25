package kubeovn_vpc_nat_gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnclient "github.com/kubeovn/kube-ovn/pkg/client/clientset/versioned/typed/kubeovn/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/harvester/terraform-provider-harvester/internal/config"
	"github.com/harvester/terraform-provider-harvester/internal/util"
	"github.com/harvester/terraform-provider-harvester/pkg/constants"
	"github.com/harvester/terraform-provider-harvester/pkg/helper"
	"github.com/harvester/terraform-provider-harvester/pkg/importer"
)

// kubeOVNProvider is kube-ovn's default subnet provider (OvnProvider in kube-ovn
// pkg/util, a package too heavy to import here).
const kubeOVNProvider = "ovn"

func ResourceKubeOVNVpcNatGateway() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceKubeOVNVpcNatGatewayCreate,
		ReadContext:   resourceKubeOVNVpcNatGatewayRead,
		UpdateContext: resourceKubeOVNVpcNatGatewayUpdate,
		DeleteContext: resourceKubeOVNVpcNatGatewayDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: Schema(),
		Timeouts: &schema.ResourceTimeout{
			Create:  schema.DefaultTimeout(2 * time.Minute),
			Read:    schema.DefaultTimeout(2 * time.Minute),
			Update:  schema.DefaultTimeout(2 * time.Minute),
			Delete:  schema.DefaultTimeout(2 * time.Minute),
			Default: schema.DefaultTimeout(2 * time.Minute),
		},
	}
}

func resourceKubeOVNVpcNatGatewayCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, err := meta.(*config.Config).K8sClient()
	if err != nil {
		return diag.FromErr(err)
	}
	name := d.Get(constants.FieldCommonName).(string)
	toCreate, err := util.ResourceConstruct(ctx, d, Creator(name))
	if err != nil {
		return diag.FromErr(err)
	}
	gateway := toCreate.(*kubeovnv1.VpcNatGateway)
	if err := setLanNetworkAttachment(ctx, c.KubeOVNClient.KubeovnV1().Subnets(), gateway); err != nil {
		return diag.FromErr(err)
	}
	obj, err := c.KubeOVNClient.KubeovnV1().VpcNatGateways().Create(ctx, gateway, metav1.CreateOptions{})
	if err != nil {
		return diag.FromErr(err)
	}
	d.SetId(helper.BuildID("", name))
	return diag.FromErr(resourceKubeOVNVpcNatGatewayImport(d, obj))
}

func resourceKubeOVNVpcNatGatewayRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, err := meta.(*config.Config).K8sClient()
	if err != nil {
		return diag.FromErr(err)
	}
	_, name, err := helper.IDParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	obj, err := c.KubeOVNClient.KubeovnV1().VpcNatGateways().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			d.SetId("")
			return nil
		}
		return diag.FromErr(err)
	}
	return diag.FromErr(resourceKubeOVNVpcNatGatewayImport(d, obj))
}

func resourceKubeOVNVpcNatGatewayUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, err := meta.(*config.Config).K8sClient()
	if err != nil {
		return diag.FromErr(err)
	}
	_, name, err := helper.IDParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	obj, err := c.KubeOVNClient.KubeovnV1().VpcNatGateways().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return diag.FromErr(err)
	}
	toUpdate, err := util.ResourceConstruct(ctx, d, Updater(obj))
	if err != nil {
		return diag.FromErr(err)
	}
	_, err = c.KubeOVNClient.KubeovnV1().VpcNatGateways().Update(ctx, toUpdate.(*kubeovnv1.VpcNatGateway), metav1.UpdateOptions{})
	if err != nil {
		return diag.FromErr(err)
	}
	return resourceKubeOVNVpcNatGatewayRead(ctx, d, meta)
}

func resourceKubeOVNVpcNatGatewayDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, err := meta.(*config.Config).K8sClient()
	if err != nil {
		return diag.FromErr(err)
	}
	_, name, err := helper.IDParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	gateways := c.KubeOVNClient.KubeovnV1().VpcNatGateways()
	// The kube-ovn webhook refuses to delete a gateway while an EIP still
	// references it, which happens while the EIPs deleted by the same destroy
	// are released: retry until it accepts. The gateway has no finalizer.
	err = retry.RetryContext(ctx, d.Timeout(schema.TimeoutDelete), func() *retry.RetryError {
		if err := gateways.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return retry.RetryableError(err)
		}
		return nil
	})
	if err != nil {
		return diag.Errorf("deleting kube-ovn VPC NAT gateway %s: %v", name, err)
	}
	d.SetId("")
	return nil
}

// setLanNetworkAttachment attaches the gateway pod to subnet when kube-ovn is a
// secondary CNI, as on Harvester. kube-ovn then adds to the pod only the
// network attachment definitions listed in the gateway's
// k8s.v1.cni.cncf.io/networks annotation, plus the external one: without it
// the pod has no interface in subnet and nothing holds lan_ip. The Harvester UI
// sets the annotation to the subnet's network attachment definition; this
// derives it the same way, from the subnet provider. Only the creation needs
// it: subnet is ForceNew and kube-ovn reads the annotation when it builds the
// gateway StatefulSet.
func setLanNetworkAttachment(ctx context.Context, subnets kubeovnclient.SubnetInterface, gateway *kubeovnv1.VpcNatGateway) error {
	subnet, err := subnets.Get(ctx, gateway.Spec.Subnet, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting subnet %s of the gateway: %w", gateway.Spec.Subnet, err)
	}
	attachment, err := lanNetworkAttachment(subnet.Spec.Provider)
	if err != nil || attachment == "" {
		return err
	}
	if gateway.Annotations == nil {
		gateway.Annotations = map[string]string{}
	}
	gateway.Annotations[nadv1.NetworkAttachmentAnnot] = attachment
	return nil
}

// lanNetworkAttachment returns the <namespace>/<name> of the network attachment
// definition behind a subnet provider <name>.<namespace>.ovn, or "" for the
// default ovn provider, whose subnets need no network attachment definition.
func lanNetworkAttachment(provider string) (string, error) {
	if provider == "" || provider == kubeOVNProvider {
		return "", nil
	}
	parts := strings.Split(provider, ".")
	if len(parts) != 3 || parts[2] != kubeOVNProvider {
		return "", fmt.Errorf("subnet provider %q is not <name>.<namespace>.%s", provider, kubeOVNProvider)
	}
	return parts[1] + "/" + parts[0], nil
}

func resourceKubeOVNVpcNatGatewayImport(d *schema.ResourceData, obj *kubeovnv1.VpcNatGateway) error {
	stateGetter, err := importer.ResourceKubeOVNVpcNatGatewayStateGetter(obj)
	if err != nil {
		return err
	}
	return util.ResourceStatesSet(d, stateGetter)
}
