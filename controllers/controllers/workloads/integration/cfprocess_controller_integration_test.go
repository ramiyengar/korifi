package integration_test

import (
	"context"
	"crypto/sha1"
	"fmt"
	"time"

	workloadsv1alpha1 "code.cloudfoundry.org/cf-k8s-controllers/controllers/apis/workloads/v1alpha1"
	. "code.cloudfoundry.org/cf-k8s-controllers/controllers/controllers/workloads/testutils"

	eiriniv1 "code.cloudfoundry.org/eirini-controller/pkg/apis/eirini/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("CFProcessReconciler Integration Tests", func() {
	var (
		testNamespace string
		ns            *corev1.Namespace

		testProcessGUID string
		testAppGUID     string
		testBuildGUID   string
		testPackageGUID string
		cfProcess       *workloadsv1alpha1.CFProcess
		cfPackage       *workloadsv1alpha1.CFPackage
		cfApp           *workloadsv1alpha1.CFApp
		cfBuild         *workloadsv1alpha1.CFBuild
	)

	const (
		CFAppGUIDLabelKey     = "workloads.cloudfoundry.org/app-guid"
		cfAppRevisionKey      = "workloads.cloudfoundry.org/app-rev"
		CFProcessGUIDLabelKey = "workloads.cloudfoundry.org/process-guid"
		CFProcessTypeLabelKey = "workloads.cloudfoundry.org/process-type"

		defaultEventuallyTimeoutSeconds = 2
		processTypeWeb                  = "web"
		processTypeWebCommand           = "bundle exec rackup config.ru -p $PORT -o 0.0.0.0"
		processTypeWorker               = "worker"
		processTypeWorkerCommand        = "bundle exec rackup config.ru"
		port8080                        = 8080
		port9000                        = 9000
	)

	BeforeEach(func() {
		ctx := context.Background()

		testNamespace = GenerateGUID()
		ns = createNamespace(ctx, k8sClient, testNamespace)

		testAppGUID = GenerateGUID()
		testProcessGUID = GenerateGUID()
		testBuildGUID = GenerateGUID()
		testPackageGUID = GenerateGUID()

		// Technically the app controller should be creating this process based on CFApp and CFBuild, but we
		// want to drive testing with a specific CFProcess instead of cascading (non-object-ref) state through
		// other resources.
		cfProcess = BuildCFProcessCRObject(testProcessGUID, testNamespace, testAppGUID, processTypeWeb, processTypeWebCommand)
		Expect(
			k8sClient.Create(ctx, cfProcess),
		).To(Succeed())

		cfApp = BuildCFAppCRObject(testAppGUID, testNamespace)
		UpdateCFAppWithCurrentDropletRef(cfApp, testBuildGUID)
		cfApp.Spec.EnvSecretName = testAppGUID + "-env"

		appEnvSecret := BuildCFAppEnvVarsSecret(testAppGUID, testNamespace, map[string]string{"test-env-key": "test-env-val"})
		Expect(
			k8sClient.Create(ctx, appEnvSecret),
		).To(Succeed())

		cfPackage = BuildCFPackageCRObject(testPackageGUID, testNamespace, testAppGUID)
		Expect(
			k8sClient.Create(ctx, cfPackage),
		).To(Succeed())
		cfBuild = BuildCFBuildObject(testBuildGUID, testNamespace, testPackageGUID, testAppGUID)
		dropletProcessTypeMap := map[string]string{
			processTypeWeb:    processTypeWebCommand,
			processTypeWorker: processTypeWorkerCommand,
		}
		dropletPorts := []int32{port8080, port9000}
		buildDropletStatus := BuildCFBuildDropletStatusObject(dropletProcessTypeMap, dropletPorts)
		cfBuild = createBuildWithDroplet(ctx, k8sClient, cfBuild, buildDropletStatus)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), ns)).To(Succeed())
	})

	When("the CFApp desired state is STARTED", func() {
		BeforeEach(func() {
			ctx := context.Background()
			cfApp.Spec.DesiredState = workloadsv1alpha1.StartedState
			Expect(
				k8sClient.Create(ctx, cfApp),
			).To(Succeed())
		})

		It("eventually reconciles the CFProcess into an LRP", func() {
			ctx := context.Background()
			var lrp eiriniv1.LRP

			Eventually(func() string {
				var lrps eiriniv1.LRPList
				err := k8sClient.List(ctx, &lrps, client.InNamespace(testNamespace))
				if err != nil {
					return ""
				}

				for _, currentLRP := range lrps.Items {
					if getMapKeyValue(currentLRP.Labels, workloadsv1alpha1.CFProcessGUIDLabelKey) == testProcessGUID {
						lrp = currentLRP
						return lrp.GetName()
					}
				}

				return ""
			}, 5*time.Second).ShouldNot(BeEmpty(), fmt.Sprintf("Timed out waiting for LRP/%s in namespace %s to be created", testProcessGUID, testNamespace))

			Expect(lrp.OwnerReferences).To(HaveLen(1), "expected length of ownerReferences to be 1")
			Expect(lrp.OwnerReferences[0].Name).To(Equal(cfProcess.Name))

			Expect(lrp.ObjectMeta.Labels).To(HaveKeyWithValue(CFAppGUIDLabelKey, testAppGUID))
			Expect(lrp.ObjectMeta.Labels).To(HaveKeyWithValue(cfAppRevisionKey, cfApp.Annotations[cfAppRevisionKey]))
			Expect(lrp.ObjectMeta.Labels).To(HaveKeyWithValue(CFProcessGUIDLabelKey, testProcessGUID))
			Expect(lrp.ObjectMeta.Labels).To(HaveKeyWithValue(CFProcessTypeLabelKey, cfProcess.Spec.ProcessType))

			Expect(lrp.Spec.GUID).To(Equal(cfProcess.Name), "Expected lrp spec GUID to match cfProcess GUID")
			Expect(lrp.Spec.Version).To(Equal(cfApp.Annotations[cfAppRevisionKey]), "Expected lrp version to match cfApp's app-rev annotation")
			Expect(lrp.Spec.DiskMB).To(Equal(cfProcess.Spec.DiskQuotaMB), "lrp DiskMB does not match")
			Expect(lrp.Spec.MemoryMB).To(Equal(cfProcess.Spec.MemoryMB), "lrp MemoryMB does not match")
			Expect(lrp.Spec.Image).To(Equal(cfBuild.Status.BuildDropletStatus.Registry.Image), "lrp Image does not match Droplet")
			Expect(lrp.Spec.ProcessType).To(Equal(processTypeWeb), "lrp process type does not match")
			Expect(lrp.Spec.AppName).To(Equal(cfApp.Spec.Name), "lrp app name does not match CFApp")
			Expect(lrp.Spec.AppGUID).To(Equal(cfApp.Name), "lrp app GUID does not match CFApp")
			Expect(lrp.Spec.Ports).To(Equal(cfProcess.Spec.Ports), "lrp ports do not match")
			Expect(lrp.Spec.Instances).To(Equal(cfProcess.Spec.DesiredInstances), "lrp desired instances does not match CFApp")
			Expect(lrp.Spec.CPUWeight).To(BeZero(), "expected cpu to always be 0")
			Expect(lrp.Spec.Sidecars).To(BeNil(), "expected sidecars to always be nil")
			Expect(lrp.Spec.Env).To(HaveKeyWithValue("test-env-key", "test-env-val"))
			Expect(lrp.Spec.Command).To(ConsistOf("/cnb/lifecycle/launcher", processTypeWebCommand))
		})

		When("a CFApp desired state is updated to STOPPED", func() {
			BeforeEach(func() {
				ctx := context.Background()

				// Wait for LRP to exist before updating CFApp
				Eventually(func() string {
					var lrps eiriniv1.LRPList
					err := k8sClient.List(ctx, &lrps, client.InNamespace(testNamespace))
					if err != nil {
						return ""
					}

					for _, currentLRP := range lrps.Items {
						if getMapKeyValue(currentLRP.Labels, workloadsv1alpha1.CFProcessGUIDLabelKey) == testProcessGUID {
							return currentLRP.GetName()
						}
					}

					return ""
				}, defaultEventuallyTimeoutSeconds*time.Second).ShouldNot(BeEmpty(), fmt.Sprintf("Timed out waiting for LRP/%s in namespace %s to be created", testProcessGUID, testNamespace))

				originalCFApp := cfApp.DeepCopy()
				cfApp.Spec.DesiredState = workloadsv1alpha1.StoppedState
				Expect(k8sClient.Patch(ctx, cfApp, client.MergeFrom(originalCFApp))).To(Succeed())
			})

			It("eventually deletes the LRPs", func() {
				ctx := context.Background()

				Eventually(func() bool {
					var lrps eiriniv1.LRPList
					err := k8sClient.List(ctx, &lrps, client.InNamespace(testNamespace))
					if err != nil {
						return false
					}

					for _, currentLRP := range lrps.Items {
						if getMapKeyValue(currentLRP.Labels, workloadsv1alpha1.CFProcessGUIDLabelKey) == testProcessGUID {
							return false
						}
					}

					return true
				}, 10*time.Second).Should(BeTrue(), "Timed out waiting for deletion of LRP/%s in namespace %s to cause NotFound error", testProcessGUID, testNamespace)
			})
		})
	})

	When("the CFApp has an LRP and is restarted by bumping the \"rev\" annotation", func() {
		var newRevValue string
		BeforeEach(func() {
			ctx := context.Background()

			appRev := cfApp.Annotations[cfAppRevisionKey]
			h := sha1.New()
			h.Write([]byte(appRev))
			appRevHash := h.Sum(nil)
			lrp1Name := testProcessGUID + fmt.Sprintf("-%x", appRevHash)[:5]
			// Create the rev1 LRP
			rev1LRP := &eiriniv1.LRP{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lrp1Name,
					Namespace: testNamespace,
					Labels: map[string]string{
						CFAppGUIDLabelKey:     cfApp.Name,
						cfAppRevisionKey:      appRev,
						CFProcessGUIDLabelKey: testProcessGUID,
						CFProcessTypeLabelKey: cfProcess.Spec.ProcessType,
					},
				},
				Spec: eiriniv1.LRPSpec{
					MemoryMB: 1,
					DiskMB:   1,
				},
			}
			Expect(
				k8sClient.Create(ctx, rev1LRP),
			).To(Succeed())

			// Set CFApp annotation.rev 0->1 & set state to started to trigger reconcile
			newRevValue = "1"
			cfApp.Annotations[cfAppRevisionKey] = newRevValue
			cfApp.Spec.DesiredState = workloadsv1alpha1.StartedState
			Expect(
				k8sClient.Create(ctx, cfApp),
			).To(Succeed())
		})

		It("deletes the old LRP, and creates a new LRP for the current revision", func() {
			// check that LRP rev1 is eventually created
			ctx := context.Background()

			var rev2LRP *eiriniv1.LRP
			Eventually(func() bool {
				var lrps eiriniv1.LRPList
				err := k8sClient.List(ctx, &lrps, client.InNamespace(testNamespace))
				if err != nil {
					return false
				}

				for _, currentLRP := range lrps.Items {
					if processGUIDLabel, has := currentLRP.Labels[CFProcessGUIDLabelKey]; has && processGUIDLabel == testProcessGUID {
						if revLabel, has := currentLRP.Labels[cfAppRevisionKey]; has && revLabel == newRevValue {
							rev2LRP = &currentLRP
							return true
						}
					}
				}
				return false
			}, defaultEventuallyTimeoutSeconds*time.Second).Should(BeTrue(), "Timed out waiting for creation of LRP/%s in namespace %s", testProcessGUID, testNamespace)

			Expect(rev2LRP.ObjectMeta.Labels).To(HaveKeyWithValue(CFAppGUIDLabelKey, testAppGUID))
			Expect(rev2LRP.ObjectMeta.Labels).To(HaveKeyWithValue(cfAppRevisionKey, cfApp.Annotations[cfAppRevisionKey]))
			Expect(rev2LRP.ObjectMeta.Labels).To(HaveKeyWithValue(CFProcessGUIDLabelKey, testProcessGUID))
			Expect(rev2LRP.ObjectMeta.Labels).To(HaveKeyWithValue(CFProcessTypeLabelKey, cfProcess.Spec.ProcessType))

			// check that LRP rev0 is eventually deleted
			Eventually(func() bool {
				var lrps eiriniv1.LRPList
				err := k8sClient.List(ctx, &lrps, client.InNamespace(testNamespace))
				if err != nil {
					return true
				}

				for _, currentLRP := range lrps.Items {
					if processGUIDLabel, has := currentLRP.Labels[CFProcessGUIDLabelKey]; has && processGUIDLabel == testProcessGUID {
						if revLabel, has := currentLRP.Labels[cfAppRevisionKey]; has && revLabel == workloadsv1alpha1.CFAppRevisionKeyDefault {
							return true
						}
					}
				}
				return false
			}, defaultEventuallyTimeoutSeconds*time.Second).Should(BeFalse(), "Timed out waiting for deletion of LRP/%s in namespace %s", testProcessGUID, testNamespace)
		})
	})

	When("the CFProcess has health check of type process", func() {
		BeforeEach(func() {
			ctx := context.Background()

			cfProcess.Spec.HealthCheck.Type = "process"
			cfProcess.Spec.Ports = []int32{}
			Expect(
				k8sClient.Update(ctx, cfProcess),
			).To(Succeed())

			cfApp.Spec.DesiredState = workloadsv1alpha1.StartedState
			Expect(
				k8sClient.Create(ctx, cfApp),
			).To(Succeed())
		})

		It("eventually reconciles the CFProcess into an LRP", func() {
			ctx := context.Background()
			var lrp eiriniv1.LRP

			Eventually(func() string {
				var lrps eiriniv1.LRPList
				err := k8sClient.List(ctx, &lrps, client.InNamespace(testNamespace))
				if err != nil {
					return ""
				}

				for _, currentLRP := range lrps.Items {
					if getMapKeyValue(currentLRP.Labels, workloadsv1alpha1.CFProcessGUIDLabelKey) == testProcessGUID {
						lrp = currentLRP
						return currentLRP.GetName()
					}
				}

				return ""
			}, 5*time.Second).ShouldNot(BeEmpty(), fmt.Sprintf("Timed out waiting for LRP/%s in namespace %s to be created", testProcessGUID, testNamespace))

			Expect(lrp.Spec.Health.Type).To(Equal(string(cfProcess.Spec.HealthCheck.Type)))
			Expect(lrp.Spec.Health.Port).To(BeZero())
		})
	})
})
