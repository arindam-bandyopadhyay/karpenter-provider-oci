/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package instancemeta

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/image"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/network"
	"github.com/oracle/karpenter-provider-oci/pkg/utils"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	corev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	SpaceBreak = " "
	ValueFalse = "false"
	ValueTrue  = "true"
)

var reservedMetadataKeys = map[string]struct{}{
	"ssh_authorized_keys": {},
	"apiserver_host":      {},
	"cluster_ca_cert":     {},
	"kubedns_svc_ip":      {},
	"kubelet-extra-args":  {},
}

type Provider interface {
	BuildInstanceMetadata(ctx context.Context, claim *corev1.NodeClaim,
		nodeClass *v1beta1.OCINodeClass,
		imageResolveResult *image.ImageResolveResult,
		networkResolveResult *network.NetworkResolveResult,
		isPreemptible bool) (map[string]string, error)
}

//nolint:unused
type DefaultProvider struct {
	ansibleBundleUrl       string
	ansiblePlayBookVersion string

	caString          string
	apiServerEndpoint string
	ipFamilies        []network.IpFamily
}

var (
	InstanceMetadataError = errors.New("cannot construct instance metadata")

	CioEverGreenOL9ImageNamePattern = regexp.MustCompile(`^(EG9[XA]|ol9_EVG)[a-zA-Z0-9_-]+`)
)

func NewProvider(ctx context.Context, apiServerEndpoint string, caCertData []byte,
	ipFamilies []network.IpFamily) (*DefaultProvider, error) {
	p := &DefaultProvider{}

	// use input caCert if it specified, ca cert is usually available in the tls config of a valid kubeconfig
	if len(caCertData) > 0 {
		p.caString = base64.StdEncoding.EncodeToString(caCertData)
	} else {
		// now try to find ca cert in a pod context, a pod has ca cert secrets mounted
		f, err := os.Open("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
		if err != nil {
			log.FromContext(ctx).Error(err,
				"instancemeta init failed, unable to open /var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
				"operation", "instancemeta_init", "outcome", "error")
			return nil, err
		}

		caBytes, err := io.ReadAll(f)
		if err != nil {
			log.FromContext(ctx).Error(err, "instancemeta init failed, unable to read ca.cert bytes",
				"operation", "instancemeta_init", "outcome", "error")
			return nil, err
		}

		p.caString = base64.StdEncoding.EncodeToString(caBytes)
	}

	// api server endpoint for nodes to talk to API server, cannot use apiServer in a pod context.
	p.apiServerEndpoint = apiServerEndpoint
	p.ipFamilies = ipFamilies

	return p, nil
}

func (p *DefaultProvider) BuildInstanceMetadata(ctx context.Context, claim *corev1.NodeClaim,
	nodeClass *v1beta1.OCINodeClass,
	imageResolveResult *image.ImageResolveResult,
	networkResolveResult *network.NetworkResolveResult,
	isPreemptible bool) (map[string]string, error) {
	if !isImageSupported(imageResolveResult) {
		err := fmt.Errorf("%w: image type %q", InstanceMetadataError, imageResolveResult.ImageType)
		log.FromContext(ctx).Error(err, "build instance metadata failed as image is not supported",
			"operation", "build_instance_metadata", "outcome", "error")
		return nil, err
	}

	// only support preBaked at this point
	metadata := make(map[string]string)

	// pass all customer provided metadata to imds metadata
	for k, v := range nodeClass.Spec.Metadata {
		if isReservedMetadataKey(k) {
			continue
		}
		metadata[k] = v
	}

	// use OKE init script as customer hasn't passed the cloud init
	if v, ok := metadata["user_data"]; !ok || v == "" {
		userData, err := p.buildUserDataForPreBakedImage(claim, nodeClass)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to build user data for preBakedImage",
				"msg", err.Error())
			return nil, err
		}
		metadata["user_data"] = userData
	} else {
		// In this case, customer pass the cloud_init script, so use it
		if nodeClass.Spec.KubeletConfig != nil && nodeClass.Spec.KubeletConfig.ClusterDNS != nil {
			metadata["kubedns_svc_ip"] = strings.Join(nodeClass.Spec.KubeletConfig.ClusterDNS, ",")
		}
		extraArgsValue, err := getKubeletExtraArgs(nodeClass, claim, p.ipFamilies)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to build kubelet extra args",
				"msg", err.Error())
			return nil, err
		}

		if extraArgsValue.Len() > 0 {
			metadata["kubelet-extra-args"] = extraArgsValue.String()
		}
		metadata["apiserver_host"] = p.apiServerEndpoint
		metadata["cluster_ca_cert"] = p.caString
	}

	injectNodeLabelsIfNeeded(metadata, nodeClass)
	injectNpnConfigIfNeeded(metadata, nodeClass, networkResolveResult, p.ipFamilies)

	// Set oke-is-preemptible metadata
	metadata["oke-is-preemptible"] = strconv.FormatBool(isPreemptible)

	// moved final metadata log to the end to include all keys (e.g., ssh_authorized_keys when present)

	if len(nodeClass.Spec.SshAuthorizedKeys) > 0 {
		metadata["ssh_authorized_keys"] = strings.Join(nodeClass.Spec.SshAuthorizedKeys, "\n")
	}

	// TODO: support [oke-is-onsr] and other attributes
	return metadata, nil
}

func isReservedMetadataKey(key string) bool {
	_, ok := reservedMetadataKeys[key]
	return ok
}

func (p *DefaultProvider) buildUserDataForPreBakedImage(claim *corev1.NodeClaim,
	nodeClass *v1beta1.OCINodeClass) (string, error) {
	var base strings.Builder
	base.WriteString("#!/usr/bin/env bash\n")
	err := appendInitScript(&base, nodeClass.Spec.PreBootstrapInitScript)
	if err != nil {
		return "", err
	}
	base.WriteString("bash /etc/oke/oke-install.sh \\\n")
	base.WriteString("--apiserver-endpoint \"")
	base.WriteString(p.apiServerEndpoint)
	base.WriteString("\" \\\n")
	base.WriteString("--kubelet-ca-cert \"")
	base.WriteString(p.caString)
	base.WriteString("\"")

	appendClusterDnsIfNeeded(&base, nodeClass)
	kubeletErr := appendKubeletExtraArgs(&base, claim, nodeClass, p.ipFamilies)
	if kubeletErr != nil {
		return "", kubeletErr
	}
	err = appendInitScript(&base, nodeClass.Spec.PostBootstrapInitScript)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(base.String())), nil
}

func injectNodeLabelsIfNeeded(metadata map[string]string, nodeClass *v1beta1.OCINodeClass) {
	if nodeClass.Spec.KubeletConfig != nil && nodeClass.Spec.KubeletConfig.NodeLabels != nil {
		metadata["oke-initial-node-labels"] = utils.PrintMapAndQuote(nodeClass.Spec.KubeletConfig.NodeLabels)
	}
}

func injectNpnConfigIfNeeded(metadata map[string]string, nodeClass *v1beta1.OCINodeClass,
	networkResolveResult *network.NetworkResolveResult, ipFamilies []network.IpFamily) {

	metadata["oke-preconfigured-vnics"] = ValueFalse
	if len(nodeClass.Spec.NetworkConfig.SecondaryVnicConfigs) > 0 && len(networkResolveResult.OtherVnicSubnets) > 0 {
		metadata["oke-preconfigured-vnics"] = ValueTrue

		maxPods := utils.GetKubeletMaxPods(nodeClass.Spec.KubeletConfig,
			nodeClass.Spec.NetworkConfig.SecondaryVnicConfigs,
			network.GetDefaultSecondaryVnicIPCount(ipFamilies))
		metadata["oke-max-pods"] = strconv.Itoa(maxPods)
		metadata["oke-native-pod-networking"] = ValueTrue
	}
}

func appendInitScript(base *strings.Builder, script *string) error {
	if script != nil {
		decoded, err := base64.StdEncoding.DecodeString(*script)
		if err != nil {
			return err
		}

		base.WriteString("\n")
		base.WriteString(string(decoded[:]))
		base.WriteString("\n")
	}
	return nil
}

// appendClusterDnsIfNeeded inject --cluster-dns as it is special handled by oke-install.sh for prebaked image
func appendClusterDnsIfNeeded(base *strings.Builder, nodeClass *v1beta1.OCINodeClass) {
	if nodeClass.Spec.KubeletConfig != nil {
		// cluster-dns & kubelet-extra-args support
		if nodeClass.Spec.KubeletConfig.ClusterDNS != nil {
			base.WriteString(" \\\n")
			base.WriteString("--cluster-dns ")
			// is this correct?
			base.WriteString(strings.Join(nodeClass.Spec.KubeletConfig.ClusterDNS, ","))
		}
	}
}

func appendKubeletExtraArgs(base *strings.Builder, claim *corev1.NodeClaim,
	class *v1beta1.OCINodeClass, ipFamilies []network.IpFamily) error {
	extraArgsValue, err := getKubeletExtraArgs(class, claim, ipFamilies)
	if err != nil {
		return err
	}

	if extraArgsValue.Len() > 0 {
		base.WriteString(" \\\n")
		base.WriteString(" --kubelet-extra-args \"")
		base.WriteString(escapeDoubleQuotedShellValue(extraArgsValue.String()))
		base.WriteString("\"")
	}

	return nil
}

func escapeDoubleQuotedShellValue(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
	).Replace(value)
}

func getKubeletExtraArgs(class *v1beta1.OCINodeClass, claim *corev1.NodeClaim,
	ipFamilies []network.IpFamily) (strings.Builder, error) {
	var extraArgsValue strings.Builder
	if class != nil && class.Spec.KubeletConfig != nil {
		kletCfg := class.Spec.KubeletConfig

		// set max pods if node class specify or npn specify
		var maxPods *int
		if kletCfg.MaxPods != nil {
			maxPods = lo.ToPtr(utils.GetKubeletMaxPods(kletCfg, class.Spec.NetworkConfig.SecondaryVnicConfigs,
				network.GetDefaultSecondaryVnicIPCount(ipFamilies)))
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--max-pods=")
			extraArgsValue.WriteString(strconv.Itoa(*maxPods))
		}

		if kletCfg.PodsPerCore != nil {
			if maxPods != nil && int32(*maxPods) < *kletCfg.PodsPerCore {
				return extraArgsValue, fmt.Errorf("PodsPerCore %d bigger than MaxPods %d", *kletCfg.PodsPerCore, *maxPods)
			}

			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--pods-per-core=")
			extraArgsValue.WriteString(strconv.Itoa(int(*kletCfg.PodsPerCore)))
		}

		if kletCfg.EvictionHard != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--eviction-hard=")
			extraArgsValue.WriteString(utils.PrintMapAndQuoteForEviction(kletCfg.EvictionHard))
		}

		if kletCfg.EvictionSoft != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--eviction-soft=")
			extraArgsValue.WriteString(utils.PrintMapAndQuoteForEviction(kletCfg.EvictionSoft))
		}

		if kletCfg.SystemReserved != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--system-reserved=")
			extraArgsValue.WriteString(utils.PrintMapAndQuote(kletCfg.SystemReserved))
		}

		if kletCfg.KubeReserved != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--kube-reserved=")
			extraArgsValue.WriteString(utils.PrintMapAndQuote(kletCfg.KubeReserved))
		}

		if kletCfg.EvictionSoftGracePeriod != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--eviction-soft-grace-period=")
			extraArgsValue.WriteString(utils.PrintMapAndQuoteDurationForEviction(kletCfg.EvictionSoftGracePeriod))
		}

		if kletCfg.EvictionMaxPodGracePeriod != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--eviction-max-pod-grace-period=")
			extraArgsValue.WriteString(strconv.Itoa(int(*kletCfg.EvictionMaxPodGracePeriod)))
		}

		if kletCfg.ImageGCHighThresholdPercent != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--image-gc-high-threshold=")
			extraArgsValue.WriteString(strconv.Itoa(int(*kletCfg.ImageGCHighThresholdPercent)))
		}

		if kletCfg.ImageGCLowThresholdPercent != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--image-gc-low-threshold=")
			extraArgsValue.WriteString(strconv.Itoa(int(*kletCfg.ImageGCLowThresholdPercent)))
		}

		if kletCfg.NodeLabels != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString("--node-labels=")
			extraArgsValue.WriteString(utils.PrintMapAndQuote(kletCfg.NodeLabels))
		}

		if kletCfg.ExtraArgs != nil {
			extraArgsValue.WriteString(SpaceBreak)
			extraArgsValue.WriteString(*kletCfg.ExtraArgs)
		}
	}

	if claim != nil {
		extraArgsValue.WriteString(SpaceBreak)
		extraArgsValue.WriteString("--register-with-taints=")

		taints := lo.Flatten([][]v1.Taint{
			claim.Spec.Taints,
			claim.Spec.StartupTaints,
		})
		if _, found := lo.Find(taints, func(t v1.Taint) bool {
			return t.MatchTaint(&corev1.UnregisteredNoExecuteTaint)
		}); !found {
			taints = append(taints, corev1.UnregisteredNoExecuteTaint)
		}
		extraArgsValue.WriteString(utils.PrintSliceAndQuoteWithControl(taints, func(item v1.Taint, _ int) string {
			if item.Value == "" {
				item.Value = "present"
			}
			return fmt.Sprintf("%s=%s:%s", item.Key, item.Value, item.Effect)
		}))
	}
	return extraArgsValue, nil
}

func isImageSupported(result *image.ImageResolveResult) bool {
	/*
	   Image support:
	   OKEImage images can get bootstrap configuration from proxymux, this is done by oke-install.sh to configure
	   proxymux cert invoke bootstrap Node package can dynamically decide to get bootstrap configuration from proxymu
	   in the absence of bootstrap config in IMDS OL7 & OL8 & other custom images: need run ansible bundle,
	   however ansible bundle does not support bootstrap (TODO)
	*/
	switch result.ImageType {
	case v1beta1.OKEImage:
		return true
	default:
		return false
	}
}
