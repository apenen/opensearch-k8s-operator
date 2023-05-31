package reconcilers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/banzaicloud/operator-tools/pkg/reconciler"
	"github.com/go-logr/logr"
	"io/ioutil"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"net/http"
	opsterv1 "opensearch.opster.io/api/v1"
	"opensearch.opster.io/pkg/helpers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"time"
)

type UpgradeCheckerReconciler struct {
	client.Client
	reconciler.ResourceReconciler
	ctx               context.Context
	recorder          record.EventRecorder
	reconcilerContext *ReconcilerContext
	instance          *opsterv1.OpenSearchCluster
	logger            logr.Logger
}

type Response struct {
	Result bool `json:"result"`
}

func NewUpgradeCheckerReconciler(
	client client.Client,
	ctx context.Context,
	recorder record.EventRecorder,
	reconcilerContext *ReconcilerContext,
	instance *opsterv1.OpenSearchCluster,
	opts ...reconciler.ResourceReconcilerOption,
) *UpgradeCheckerReconciler {
	return &UpgradeCheckerReconciler{
		Client: client,
		ResourceReconciler: reconciler.NewReconcilerWith(client,
			append(
				opts,
				reconciler.WithPatchCalculateOptions(patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(), patch.IgnoreStatusFields()),
				reconciler.WithLog(log.FromContext(ctx).WithValues("reconciler", "UpgradeChecker")),
			)...),
		ctx:               ctx,
		recorder:          recorder,
		reconcilerContext: reconcilerContext,
		instance:          instance,
		logger:            log.FromContext(ctx),
	}
}

type Payload struct {
	UID                string   `json:"uid"`
	OperatorVersion    string   `json:"operatorVersion"`
	ClusterCount       int      `json:"clusterCount"`
	OsClustersVersions []string `json:"osClustersVersions"`
}

func (r *UpgradeCheckerReconciler) Reconcile() (ctrl.Result, error) {
	requeue := false
	var err error
	var Builtjson []byte
	results := reconciler.CombinedResult{}
	//if !isTimeToRunFunction() {
	//	results.Combine(&ctrl.Result{Requeue: requeue}, nil)
	//	return results.Result, nil
	//}

	Builtjson, err = r.BuildJSONPayload()
	if err != nil {
		results.Combine(&ctrl.Result{Requeue: requeue}, err)
		return results.Result, results.Err
	}
	if err != nil {
		results.Combine(&ctrl.Result{Requeue: requeue}, err)
		return results.Result, results.Err
	}

	serverURL := "http://localhost:1111/operator-usage"
	response, err := r.sendJSONToServer(Builtjson, serverURL)

	// if err != nil so I didnt got a response
	if err != nil {
		fmt.Println("Failed to send JSON payload:", err)
		results.Combine(&ctrl.Result{Requeue: requeue}, err)
		return results.Result, results.Err
	}

	// if respnse == nil and no error so the Operator is Up to date (cause the server is not returning anything when the Version is latest).
	if response == true && err == nil {
		// Operator is up to date
		results.Combine(&ctrl.Result{Requeue: requeue}, nil)
		return results.Result, results.Err
	}

	// Log for the client, you are not up to date
	r.logger.Info("Notice - Your Operator deployment is not up to date, follow the instructions on ArtifactHUB.io page https://artifacthub.io/packages/helm/opensearch-operator/opensearch-operator ")
	results.Combine(&ctrl.Result{Requeue: requeue}, nil)
	return results.Result, results.Err
}

func isTimeToRunFunction() bool {
	now := time.Now()
	return now.Hour() == 12 && now.Minute() == 0 && now.Second() == 0
}

func (r *UpgradeCheckerReconciler) BuildJSONPayload() ([]byte, error) {
	var versions []string
	var ClusterCount int
	myUid, operatorNamespace, err := r.FindUidFromSecret(r.ctx)
	if err != nil {
		return []byte{}, err
	}

	OperatorVersion, err := r.FindOperatorVersion(r.ctx, r.Client, operatorNamespace)
	if err != nil {
		return []byte{}, err
	}

	ClusterCount, versions, err = r.FindCountOfOsClusterAndVersions(r.ctx, r.Client)
	if err != nil {
		return []byte{}, err
	}
	Pay := Payload{
		UID:                myUid,
		OperatorVersion:    OperatorVersion,
		ClusterCount:       ClusterCount,
		OsClustersVersions: versions,
	}

	jsonData, err := ConvertToJSON(Pay)
	if err != nil {
		return jsonData, err
	}
	return jsonData, nil

}

func ConvertToJSON(pay Payload) ([]byte, error) {
	jsonData, err := json.Marshal(pay)
	fmt.Println(jsonData)
	fmt.Println(pay)
	if err != nil {
		return []byte{}, err
	}
	return jsonData, nil
}

func (r *UpgradeCheckerReconciler) FindUidFromSecret(ctx context.Context) (string, string, error) {

	secretList := &v1.SecretList{}
	var valueStr string
	var namespace string
	if err := r.List(ctx, secretList); err != nil {
		r.logger.Error(err, "Cannot find UID secret")
		return "-1", "-1", err
		// Handle the error
	}

	for _, secret := range secretList.Items {
		if secret.Name == "operator-uid" {
			value, ok := secret.Data["secretKey"]
			if !ok {
				r.logger.Info("Cannot secretKey inside of UID secret")
			}
			valueStr = string(value)
			namespace = secret.Namespace
			//r.logger.Info("UID:", valueStr)
			break
		}
	}

	return valueStr, namespace, nil
}

func (r *UpgradeCheckerReconciler) FindOperatorVersion(ctx context.Context, k8sClient client.Client, operatorNamespace string) (string, error) {
	deployOperator := &appsv1.Deployment{}
	var imageVersion string
	err := k8sClient.Get(ctx, client.ObjectKey{Name: "opensearch-operator-controller-manager", Namespace: operatorNamespace}, deployOperator)
	if err != nil {
		r.logger.Error(err, "Cannot find Operator Deployment")
		return "0", err
	}

	for i := 0; i < len(deployOperator.Spec.Template.Spec.Containers); i++ {
		imageVersion = deployOperator.Spec.Template.Spec.Containers[i].Image
		if strings.Contains(imageVersion, "opensearch-operator") {
			break
		}

	}

	version := helpers.FindVersion(imageVersion)
	return version, err

}

func (r *UpgradeCheckerReconciler) FindCountOfOsClusterAndVersions(ctx context.Context, k8sClient client.Client) (int, []string, error) {
	var empty []string
	list := &opsterv1.OpenSearchClusterList{}
	if err := k8sClient.List(ctx, list); err != nil {
		r.logger.Error(err, "Cannot find the CRD instances ")
		return 0, empty, err
	}
	var clustersVersion []string
	for cluster := 0; cluster < len(list.Items); cluster++ {
		clustersVersion = append(clustersVersion, list.Items[cluster].Spec.General.Version)
	}

	return len(list.Items), clustersVersion, nil
}

func (r *UpgradeCheckerReconciler) sendJSONToServer(jsonPayload []byte, serverURL string) (bool, error) {
	retries := 5
	timeout := 15 * time.Second

	client := http.Client{
		Timeout: timeout,
	}

	fmt.Println(string(jsonPayload))
	for attempt := 1; attempt <= retries; attempt++ {
		req, err := http.NewRequest("POST", serverURL, bytes.NewBuffer(jsonPayload))
		if err != nil {
			return false, err
		}
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")

		// run the actule request
		resp, err := client.Do(req)

		if err == nil && resp.Status == "200 OK" {
			defer resp.Body.Close()
			body, _ := ioutil.ReadAll(resp.Body)
			fmt.Println("response Status:", resp.Status)
			fmt.Println("response Headers:", resp.Header)
			fmt.Println("response Body:", string(body))
			var response Response
			err = json.Unmarshal(body, &response)
			if err != nil {
				return false, err
			}
			if response.Result {
				return true, nil
			} else {
				if !response.Result {
					return false, nil
				}
			}
			if err != nil {
				return false, err
			}
		} else {
			if err != nil {
				return false, err

			}
		}
		time.Sleep(timeout)
	}

	return false, nil
}