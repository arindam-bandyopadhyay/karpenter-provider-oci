/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package orphaninstance

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	ociv1beta1 "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/fakes"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	k8sclientfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerclientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var c *Controller
var fakeControllerClient client.Client
var fakeKubernetesInterface kubernetes.Interface
var fakeCloudProvider *fakes.FakeCloudProvider
var ctx = context.Background()

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

//nolint:dupl
var _ = Describe("orphanInstanceController", func() {
	BeforeEach(func() {
		lo.Must0(clientgoscheme.AddToScheme(clientgoscheme.Scheme))
		lo.Must0(ociv1beta1.AddToScheme(clientgoscheme.Scheme))

		// 2. Create the fake client, optionally with initial objects
		fakeControllerClient = controllerclientfake.NewClientBuilder().
			WithScheme(clientgoscheme.Scheme).
			WithIndex(&v1.Node{}, "spec.providerID", func(obj client.Object) []string {
				node, ok := obj.(*v1.Node)
				if !ok {
					return nil
				}
				if node.Spec.ProviderID == "" {
					return nil
				}
				return []string{node.Spec.ProviderID}
			}).
			WithIndex(&v1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
				pod, ok := obj.(*v1.Pod)
				if !ok {
					return nil
				}

				if pod.Spec.NodeName == "" {
					return nil
				}

				return []string{pod.Spec.NodeName}
			}).
			// WithObjects(myInitialObject, anotherObject).
			Build()

		fakeKubernetesInterface = k8sclientfake.NewClientset()

		fakeCloudProvider = &fakes.FakeCloudProvider{}
		fakeCloudProvider.GetSupportedNodeClassesStub = func() []status.Object {
			return []status.Object{&ociv1beta1.OCINodeClass{}}
		}

		c = NewController(context.Background(), fakeControllerClient, fakeKubernetesInterface, fakeCloudProvider)
	})

	It("dummy run", func() {
		fakeCloudProvider.ListStub = func(ctx context.Context) ([]*corev1.NodeClaim, error) {
			return nil, nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
	})

	It("node claim matches", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "id2")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc3", "")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id2"),
				miniNodeClaims("nc4", ""),
			}, nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
	})

	It("node claim matches", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "id2")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc3", "")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id2"),
				miniNodeClaims("nc4", ""),
			}, nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
	})

	It("node claim mismatch, cloud has provider id, node claim is not tracked, node not registered", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id2"),
			}, nil
		}

		var count = 0
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			count++
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
		Expect(count).Should(Equal(1))
	})

	It("node claim mismatch, cloud has provider id and just created, node claim exit but no providerID", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			nodeClaimJustCreated := miniNodeClaims("nc2", "id2")
			nodeClaimJustCreated.CreationTimestamp = metav1.NewTime(time.Now().Add(creationTimeThreshold - 1))
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				nodeClaimJustCreated,
			}, nil
		}

		var count = 0
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			count++
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
		Expect(count).Should(Equal(0))
	})

	It("node claim mismatch, cloud has provider id and just created, node claim not exist", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			nodeClaimJustCreated := miniNodeClaims("nc3", "id2")
			nodeClaimJustCreated.CreationTimestamp = metav1.NewTime(time.Now().Add(creationTimeThreshold - 1))
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				nodeClaimJustCreated,
			}, nil
		}

		var count = 0
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			count++
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
		Expect(count).Should(Equal(1))
	})

	It("node claim mismatch, cloud has provider id different from node claim, node not registered", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "id2")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id3"),
			}, nil
		}

		var count = 0
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			count++
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))
		Expect(count).Should(Equal(1))
	})

	It("node claim mismatch, cloud has provider id different from node claim, node registered", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "id2")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id3"),
			}, nil
		}

		lo.Must0(fakeControllerClient.Create(ctx, miniNode("nc2", "id3")))

		var count = 0
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			count++
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))

		node := &v1.Node{}
		getErr := fakeControllerClient.Get(ctx, client.ObjectKey{
			Name: "nc2",
		}, node)
		Expect(getErr).ShouldNot(HaveOccurred())
		Expect(node.Spec.Unschedulable).Should(BeTrue())
		Expect(count).Should(Equal(1))
	})

	It("node claim mismatch, cloud has provider id different from node claim, node registered, pods running",
		func() {
			lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
			lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "id2")))

			fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
				return []*corev1.NodeClaim{
					miniNodeClaims("nc1", "id1"),
					miniNodeClaims("nc2", "id3"),
				}, nil
			}

			lo.Must0(fakeControllerClient.Create(ctx, miniNode("nc2", "id3")))

			pod1 := miniPod("pod1", "nc2")
			pod2 := miniPod("pod2", "nc2")
			lo.Must0(fakeControllerClient.Create(ctx, pod1))
			lo.Must0(fakeControllerClient.Create(ctx, pod2))

			_, err := fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod1,
				metav1.CreateOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			_, err = fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod2,
				metav1.CreateOptions{})
			Expect(err).ShouldNot(HaveOccurred())

			var count = 0
			fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
				count++
				return nil
			}

			requeue, err := c.Reconcile(ctx)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(requeue.RequeueAfter).Should(Equal(fastRequeueAfter))

			node := &v1.Node{}
			getErr := fakeControllerClient.Get(ctx, client.ObjectKey{
				Name: "nc2",
			}, node)
			Expect(getErr).ShouldNot(HaveOccurred())
			Expect(node.Spec.Unschedulable).Should(BeTrue())
			Expect(count).Should(Equal(0))
		})

	It("preserves multiple reclaim errors from parallel workers", func() {
		err1 := errors.New("delete nc1")
		err2 := errors.New("delete nc2")

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id2"),
				miniNodeClaims("nc3", "id3"),
			}, nil
		}
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			switch claim.Name {
			case "nc1":
				return err1
			case "nc2":
				return err2
			default:
				return nil
			}
		}

		_, err := c.Reconcile(ctx)
		Expect(err).Should(HaveOccurred())
		Expect(errors.Is(err, err1)).Should(BeTrue())
		Expect(errors.Is(err, err2)).Should(BeTrue())
	})

	It("returns fast requeue when any parallel reclaim is unfinished", func() {
		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc-draining", "id-draining"),
				miniNodeClaims("nc-delete", "id-delete"),
			}, nil
		}

		lo.Must0(fakeControllerClient.Create(ctx, miniNode("nc-draining", "id-draining")))
		pod := miniPod("pod1", "nc-draining")
		lo.Must0(fakeControllerClient.Create(ctx, pod))
		_, err := fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		var deleteCount atomic.Int32
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			deleteCount.Add(1)
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(fastRequeueAfter))
		Expect(deleteCount.Load()).Should(Equal(int32(1)))

		node := &v1.Node{}
		getErr := fakeControllerClient.Get(ctx, client.ObjectKey{
			Name: "nc-draining",
		}, node)
		Expect(getErr).ShouldNot(HaveOccurred())
		Expect(node.Spec.Unschedulable).Should(BeTrue())
	})

	It("node claim mismatch, cloud has provider id different from node claim,"+
		" node registered, non evictable pods running", func() {
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc1", "id1")))
		lo.Must0(fakeControllerClient.Create(ctx, miniNodeClaims("nc2", "id2")))

		fakeCloudProvider.ListStub = func(ctx2 context.Context) ([]*corev1.NodeClaim, error) {
			return []*corev1.NodeClaim{
				miniNodeClaims("nc1", "id1"),
				miniNodeClaims("nc2", "id3"),
			}, nil
		}

		lo.Must0(fakeControllerClient.Create(ctx, miniNode("nc2", "id3")))

		pod1 := miniPod("pod1", "nc2")
		pod1.Status.Phase = v1.PodFailed
		pod2 := miniPod("pod2", "nc2")
		pod2.Status.Phase = v1.PodSucceeded
		pod3 := miniPod("pod3", "nc2")
		pod3.ObjectMeta.OwnerReferences = append(pod3.ObjectMeta.OwnerReferences, metav1.OwnerReference{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
		})
		pod4 := miniPod("pod4", "nc2")
		pod4.ObjectMeta.Annotations = map[string]string{
			"kubernetes.io/config.mirror": "true",
		}
		pod5 := miniPod("pod5", "nc1")

		lo.Must0(fakeControllerClient.Create(ctx, pod1))
		lo.Must0(fakeControllerClient.Create(ctx, pod2))
		lo.Must0(fakeControllerClient.Create(ctx, pod3))
		lo.Must0(fakeControllerClient.Create(ctx, pod4))
		lo.Must0(fakeControllerClient.Create(ctx, pod5))

		_, err := fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod1, metav1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())
		_, err = fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod2, metav1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())
		_, err = fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod3, metav1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())
		_, err = fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod4, metav1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())
		_, err = fakeKubernetesInterface.CoreV1().Pods("default").Create(ctx, pod5, metav1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		var count = 0
		fakeCloudProvider.DeleteStub = func(ctx2 context.Context, claim *corev1.NodeClaim) error {
			count++
			return nil
		}

		requeue, err := c.Reconcile(ctx)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(requeue.RequeueAfter).Should(Equal(slowRequeueAfter))

		node := &v1.Node{}
		getErr := fakeControllerClient.Get(ctx, client.ObjectKey{
			Name: "nc2",
		}, node)
		Expect(getErr).ShouldNot(HaveOccurred())
		Expect(node.Spec.Unschedulable).Should(BeTrue())
		Expect(count).Should(Equal(1))
	})

})

func miniNodeClaims(name string, providerId string) *corev1.NodeClaim {
	nc := &corev1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: corev1.NodeClaimSpec{
			NodeClassRef: &corev1.NodeClassReference{
				Group: ociv1beta1.Group,
				Kind:  "OCINodeClass",
			},
		},
		Status: corev1.NodeClaimStatus{
			ProviderID: providerId,
		},
	}

	return nc
}

func miniNode(name string, providerId string) *v1.Node {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.NodeSpec{
			ProviderID:    providerId,
			Unschedulable: false,
		},
	}

	return node
}

func miniPod(name string, nodeName string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
		},
	}

	return pod
}
