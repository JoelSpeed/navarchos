package handler

import (
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	navarchosv1alpha1 "github.com/pusher/navarchos/pkg/apis/navarchos/v1alpha1"
	"github.com/pusher/navarchos/pkg/controller/nodereplacement/status"
	"github.com/pusher/navarchos/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var _ = Describe("new node replacement handler", func() {
	var m utils.Matcher
	var h *NodeReplacementHandler
	var opts *Options

	var nodeReplacement *navarchosv1alpha1.NodeReplacement
	var mgrStopped *sync.WaitGroup
	var stopMgr chan struct{}

	var workerNode1 *corev1.Node
	var workerNode2 *corev1.Node
	var pod1 *corev1.Pod
	var pod2 *corev1.Pod
	var pod3 *corev1.Pod
	var pod4 *corev1.Pod

	const timeout = time.Second * 5
	const consistentlyTimeout = time.Second

	var newPod = func(name string, node *corev1.Node) *corev1.Pod {
		pod := utils.ExamplePod.DeepCopy()
		pod.Name = name
		pod.Spec.NodeName = node.Name
		return pod
	}

	var setPodRunning = func(obj utils.Object) utils.Object {
		pod, _ := obj.(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return pod
	}

	var setPodPending = func(obj utils.Object) utils.Object {
		pod, _ := obj.(*corev1.Pod)
		pod.Status.Phase = corev1.PodPending
		return pod
	}

	var setPodSucceeded = func(obj utils.Object) utils.Object {
		pod, _ := obj.(*corev1.Pod)
		pod.Status.Phase = corev1.PodSucceeded
		return pod
	}

	BeforeEach(func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		c, err := client.New(cfg, client.Options{})
		Expect(err).ToNot(HaveOccurred())
		m = utils.Matcher{Client: c}

		stopMgr, mgrStopped = StartTestManager(mgr)

		grace := 5 * time.Second
		opts = &Options{
			EvictionGracePeriod: &grace,
		}

		// Create nodes to act as owners for the NodeReplacements created
		workerNode1 = utils.ExampleNodeWorker1.DeepCopy()
		workerNode2 = utils.ExampleNodeWorker2.DeepCopy()
		m.Create(workerNode1).Should(Succeed())
		m.Create(workerNode2).Should(Succeed())

		// Create some pods attached to the nodes
		pod1 = newPod("pod-1", workerNode1)
		pod2 = newPod("pod-2", workerNode1)
		pod3 = newPod("pod-3", workerNode1)
		pod4 = newPod("pod-4", workerNode2)
		m.Create(pod1).Should(Succeed())
		m.Create(pod2).Should(Succeed())
		m.Create(pod3).Should(Succeed())
		m.Create(pod4).Should(Succeed())
		m.UpdateStatus(pod1, setPodRunning, timeout).Should(Succeed())
		m.UpdateStatus(pod2, setPodRunning, timeout).Should(Succeed())
		m.UpdateStatus(pod3, setPodRunning, timeout).Should(Succeed())
		m.UpdateStatus(pod4, setPodRunning, timeout).Should(Succeed())

		nodeReplacement = utils.ExampleNodeReplacement.DeepCopy()
		nodeReplacement.SetOwnerReferences([]metav1.OwnerReference{utils.GetOwnerReferenceForNode(workerNode1)})
		nodeReplacement.Spec.ReplacementSpec.Priority = intPtr(0)
		nodeReplacement.Spec.NodeUID = workerNode1.GetUID()
		nodeReplacement.Spec.NodeName = workerNode1.GetName()
		m.Create(nodeReplacement).Should(Succeed())
	})

	AfterEach(func() {
		close(stopMgr)
		mgrStopped.Wait()

		pods := &corev1.PodList{}
		m.List(pods).Should(Succeed())
		for _, pod := range pods.Items {
			m.UpdateStatus(&pod, setPodSucceeded, timeout).Should(Succeed())
		}

		utils.DeleteAll(cfg, timeout,
			&navarchosv1alpha1.NodeReplacementList{},
			&corev1.NodeList{},
			&corev1.PodList{},
			&appsv1.DaemonSetList{},
			&policyv1beta1.PodDisruptionBudgetList{},
		)

		m.Eventually(&corev1.PodList{}, timeout).Should(utils.WithListItems(BeEmpty()))
	})

	JustBeforeEach(func() {
		h = NewNodeReplacementHandler(m.Client, opts)
	})

	Context("shouldRequeueReplacement", func() {
		var requeue bool
		var reason string

		JustBeforeEach(func() {
			requeue, reason = h.shouldRequeueReplacement(nodeReplacement)
		})
		Context("if a another NodeReplacement is higher priority", func() {
			var highPriorityNR *navarchosv1alpha1.NodeReplacement
			BeforeEach(func() {
				highPriorityNR = utils.ExampleNodeReplacement.DeepCopy()
				highPriorityNR.SetName("high-priority")
				highPriorityNR.Spec.ReplacementSpec.Priority = intPtr(10)
				highPriorityNR.SetOwnerReferences([]metav1.OwnerReference{utils.GetOwnerReferenceForNode(workerNode2)})
				m.Create(highPriorityNR).Should(Succeed())
				m.Update(nodeReplacement, func(obj utils.Object) utils.Object {
					nr, _ := obj.(*navarchosv1alpha1.NodeReplacement)
					nr.Spec.ReplacementSpec.Priority = intPtr(0)
					return nr
				}, timeout).Should(Succeed())
				m.Eventually(nodeReplacement, timeout).Should(utils.WithField("Spec.ReplacementSpec.Priority", Equal(intPtr(0))))
				Expect(*highPriorityNR.Spec.ReplacementSpec.Priority).To(BeNumerically(">", *nodeReplacement.Spec.ReplacementSpec.Priority))
			})

			It("sets requeue to true", func() {
				Expect(requeue).To(BeTrue())
			})

			It("requeues the NodeReplacement in the result", func() {
				Expect(reason).To(Equal("NodeReplacement \"high-priority\" has a higher priority"))
			})

			Context("and the higher priority replacement has completed", func() {
				BeforeEach(func() {
					m.Update(highPriorityNR, func(obj utils.Object) utils.Object {
						nr, _ := obj.(*navarchosv1alpha1.NodeReplacement)
						nr.Status.Phase = navarchosv1alpha1.ReplacementPhaseCompleted
						return nr
					}, timeout).Should(Succeed())
					m.Eventually(highPriorityNR, timeout).Should(utils.WithField("Status.Phase", Equal(navarchosv1alpha1.ReplacementPhaseCompleted)))
				})

				It("sets requeue to false", func() {
					Expect(requeue).To(BeFalse())
				})

				It("does not set the reason string", func() {
					Expect(reason).To(Equal(""))
				})
			})

		})

		Context("if a another NodeReplacement is in Phase InProgress", func() {
			BeforeEach(func() {
				inProgressNR := utils.ExampleNodeReplacement.DeepCopy()
				inProgressNR.SetName("in-progress")
				inProgressNR.Status.Phase = navarchosv1alpha1.ReplacementPhaseInProgress
				inProgressNR.SetOwnerReferences([]metav1.OwnerReference{utils.GetOwnerReferenceForNode(workerNode2)})
				m.Create(inProgressNR).Should(Succeed())
			})

			It("sets requeue to true", func() {
				Expect(requeue).To(BeTrue())
			})

			It("requeues the NodeReplacement", func() {
				Expect(reason).To(Equal("NodeReplacement \"in-progress\" is already in-progress"))
			})
		})

		Context("if a pod is pending", func() {
			BeforeEach(func() {
				m.UpdateStatus(pod1, setPodPending, timeout).Should(Succeed())
			})

			It("sets requeue to true", func() {
				Expect(requeue).To(BeTrue())
			})

			It("requeues the NodeReplacement", func() {
				Expect(reason).To(Equal("requeuing as there are pending pod(s): [pod-1]"))
			})
		})

		Context("if the NodeReplacement should not be requeued", func() {
			It("sets requeue to false", func() {
				Expect(requeue).To(BeFalse())
			})

			It("does not set the reason string", func() {
				Expect(reason).To(Equal(""))
			})
		})
	})

	Context("getNode", func() {
		var node *corev1.Node
		var exists bool
		var existsErr error

		JustBeforeEach(func() {
			node, exists, existsErr = h.getNode(nodeReplacement)
		})

		Context("when the node does not exist", func() {
			BeforeEach(func() {
				m.Update(nodeReplacement, func(obj utils.Object) utils.Object {
					nr, _ := obj.(*navarchosv1alpha1.NodeReplacement)
					nr.Spec.NodeName = "does-not-exist"
					return nr
				}, timeout).Should(Succeed())
			})

			It("sets exists to false", func() {
				Expect(exists).To(BeFalse())
			})

			It("does not set an error", func() {
				Expect(existsErr).ToNot(HaveOccurred())
			})

			It("does not return a node", func() {
				Expect(node).To(BeNil())
			})
		})

		Context("when the node exists", func() {
			Context("and the UIDs match", func() {
				It("sets exists to true", func() {
					Expect(exists).To(BeTrue())
				})

				It("does not set an error", func() {
					Expect(existsErr).ToNot(HaveOccurred())
				})

				It("returns the node", func() {
					Expect(node.GetName()).To(Equal(workerNode1.GetName()))
					Expect(node.GetUID()).To(Equal(workerNode1.GetUID()))
				})
			})

			Context("and the UIDs do not match", func() {
				BeforeEach(func() {
					m.Update(nodeReplacement, func(obj utils.Object) utils.Object {
						nr, _ := obj.(*navarchosv1alpha1.NodeReplacement)
						nr.Spec.NodeUID = "does-not-match"
						return nr
					}, timeout).Should(Succeed())
				})

				It("sets exists to false", func() {
					Expect(exists).To(BeFalse())
				})

				It("does not set an error", func() {
					Expect(existsErr).ToNot(HaveOccurred())
				})

				It("does not return a node", func() {
					Expect(node).To(BeNil())
				})
			})
		})
	})

	Context("cordonNode", func() {
		var err error
		JustBeforeEach(func() {
			err = h.cordonNode(workerNode1)
		})

		Context("when called on an uncordoned node", func() {
			It("should cordon the node", func() {
				m.Eventually(workerNode1, timeout).Should(SatisfyAll(
					utils.WithField("Spec.Unschedulable", BeTrue()),
					utils.WithField("Spec.Taints",
						ContainElement(SatisfyAll(
							utils.WithField("Effect", Equal(corev1.TaintEffect("NoSchedule"))),
							utils.WithField("Key", Equal("node.kubernetes.io/unschedulable")),
							utils.WithField("TimeAdded", Not(BeNil())),
						)),
					)))
			})

			It("should not return an error", func() {
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when called on a cordoned node", func() {
			var taint corev1.Taint

			BeforeEach(func() {
				now := metav1.Now()
				taint = corev1.Taint{
					Key:       "node.kubernetes.io/unschedulable",
					Effect:    corev1.TaintEffect("NoSchedule"),
					TimeAdded: &now,
				}

				m.Update(workerNode1, func(obj utils.Object) utils.Object {
					node, _ := obj.(*corev1.Node)
					node.Spec.Unschedulable = true

					node.Spec.Taints = []corev1.Taint{taint}
					return node
				}, timeout).Should(Succeed())
			})

			It("should not update the node", func() {
				m.Consistently(workerNode1, timeout).Should(SatisfyAll(
					utils.WithField("Spec.Unschedulable", BeTrue()),
					utils.WithField("Spec.Taints", ConsistOf(taint)),
				))
			})

			It("should not return an error", func() {
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Context("getPodsOnNode", func() {
		var nodePods []string
		var ignoredPods []navarchosv1alpha1.PodReason
		var err error

		JustBeforeEach(func() {
			nodePods, ignoredPods, err = h.getPodsOnNode(workerNode1)
		})

		Context("when a pod is owned by a daemonset", func() {
			var daemonset *appsv1.DaemonSet
			BeforeEach(func() {
				daemonset = utils.ExampleDaemonSet.DeepCopy()
				m.Create(daemonset).Should(Succeed())
				m.Update(pod2, func(obj utils.Object) utils.Object {
					pod, _ := obj.(*corev1.Pod)
					pod.SetOwnerReferences([]metav1.OwnerReference{utils.GetOwnerReferenceForDaemonSet(daemonset)})
					return pod
				}, timeout).Should(Succeed())
			})

			It("sets NodePods", func() {
				Expect(nodePods).To(ConsistOf(
					"pod-1",
					"pod-2",
					"pod-3",
				))
			})

			It("sets IgnoredPods", func() {
				Expect(ignoredPods).To(ConsistOf(
					navarchosv1alpha1.PodReason{Name: "pod-2", Reason: "pod owned by a DaemonSet"}))
			})

			It("should not return an error", func() {
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when no pod is owned by a daemonset", func() {
			It("sets NodePods", func() {
				Expect(nodePods).To(ConsistOf(
					"pod-1",
					"pod-2",
					"pod-3",
				))
			})

			It("sets IgnoredPods", func() {
				Expect(ignoredPods).To(BeEmpty())
			})

			It("should not return an error", func() {
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Context("when handleNew is called on a New NodeReplacement", func() {
		var result *status.Result
		var handleErr error

		JustBeforeEach(func() {
			result, handleErr = h.handleNew(nodeReplacement)
		})

		It("should not set any Pods in the EvictedPods field", func() {
			Expect(result.EvictedPods).To(BeEmpty())
		})

		It("should not evict any pods", func() {
			for _, pod := range []*corev1.Pod{pod1, pod2, pod3, pod4} {
				m.Consistently(pod).Should(utils.WithField("ObjectMeta.DeletionTimestamp", BeNil()))
			}
		})

		It("should set the NodeCordonReason to NodeCordoned", func() {
			Expect(result.NodeCordonReason).To(Equal(navarchosv1alpha1.ReasonNodeCordoned))
		})

		It("should not return an error", func() {
			Expect(handleErr).ToNot(HaveOccurred())
		})
	})
})
