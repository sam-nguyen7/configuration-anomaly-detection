// Package investigate holds the investigate command
/*
Copyright © 2022 Red Hat, Inc.

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
package investigate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	investigation "github.com/openshift/configuration-anomaly-detection/pkg/investigations"
	"github.com/openshift/configuration-anomaly-detection/pkg/investigations/ccam"
	"github.com/openshift/configuration-anomaly-detection/pkg/investigations/chgm"
	"github.com/openshift/configuration-anomaly-detection/pkg/investigations/cpd"
	"github.com/openshift/configuration-anomaly-detection/pkg/logging"
	"github.com/openshift/configuration-anomaly-detection/pkg/managedcloud"
	"github.com/openshift/configuration-anomaly-detection/pkg/metrics"
	ocm "github.com/openshift/configuration-anomaly-detection/pkg/ocm"
	"github.com/openshift/configuration-anomaly-detection/pkg/pagerduty"
	"github.com/openshift/configuration-anomaly-detection/pkg/utils"

	"github.com/spf13/cobra"
)

// InvestigateCmd represents the entry point for alert investigation
var InvestigateCmd = &cobra.Command{
	Use:          "investigate",
	SilenceUsage: true,
	Short:        "Filter for and investigate supported alerts",
	RunE:         run,
}

var (
	logLevelString = "info"
	payloadPath    = "./payload.json"
)

func init() {
	InvestigateCmd.Flags().StringVarP(&payloadPath, "payload-path", "p", payloadPath, "the path to the payload")
	InvestigateCmd.Flags().StringVarP(&logLevelString, "log-level", "l", logLevelString, "the log level [debug,info,warn,error,fatal], default = info")

	err := InvestigateCmd.MarkFlagRequired("payload-path")
	if err != nil {
		logging.Warn("Could not mark flag 'payload-path' as required")
	}
}

func run(_ *cobra.Command, _ []string) error {
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		return fmt.Errorf("failed to read webhook payload: %w", err)
	}
	logging.Info("Running CAD with webhook payload:", string(payload))

	pdClient, err := GetPDClient(payload)
	if err != nil {
		return fmt.Errorf("could not initialize pagerduty client: %w", err)
	}

	logging.Infof("Incident link: %s", pdClient.GetIncidentRef())

	alertType := isAlertSupported(pdClient.GetTitle())
	alertTypeString := alertType.String()
	metrics.Inc(metrics.Alerts, alertTypeString, pdClient.GetEventType())

	// Escalate all unsupported alerts
	if alertType == investigation.Unsupported {
		err = pdClient.EscalateAlert()
		if err != nil {
			return fmt.Errorf("Could not escalate unsupported alert: %w", err)
		}
		return nil
	}

	// clusterID can end up being either be the internal or external ID.
	// We don't really care, as we only use this to initialize the cluster object,
	// which will contain both IDs.
	clusterID, err := pdClient.RetrieveClusterID()
	if err != nil {
		return err
	}

	ocmClient, err := GetOCMClient()
	if err != nil {
		return fmt.Errorf("could not initialize ocm client: %w", err)
	}

	cluster, err := ocmClient.GetClusterInfo(clusterID)
	if err != nil {
		return fmt.Errorf("could not retrieve cluster info for %s: %w", clusterID, err)
	}
	// From this point on, we normalize to internal ID, as this ID always exists.
	// For installing clusters, externalID can be empty.
	internalClusterID := cluster.ID()

	// initialize logger for the internal-cluster-id context
	logging.RawLogger = logging.InitLogger(logLevelString, internalClusterID)

	requiresInvestigation, err := clusterRequiresInvestigation(cluster, ocmClient, pdClient)
	if err != nil || !requiresInvestigation {
		return err
	}

	clusterDeployment, err := ocmClient.GetClusterDeployment(internalClusterID)
	if err != nil {
		return fmt.Errorf("could not retrieve Cluster Deployment for %s: %w", internalClusterID, err)
	}

	customerAwsClient, err := managedcloud.CreateCustomerAWSClient(cluster, ocmClient)
	if err != nil {
		return ccam.Evaluate(cluster, err, ocmClient, pdClient, alertTypeString)
	}

	logging.Info("Successfully accessed customer cloud account. Removing existing 'Cloud Credentials Are Missing' limited support reasons.")
	err = ccam.RemoveLimitedSupport(cluster, ocmClient, pdClient, alertTypeString)
	if err != nil {
		return err
	}

	investigationResources := &investigation.Resources{AlertType: alertType, Cluster: cluster, ClusterDeployment: clusterDeployment, AwsClient: customerAwsClient, OcmClient: ocmClient, PdClient: pdClient}

	run := investigation.NewInvestigation()

	switch alertType {
	case investigation.ClusterHasGoneMissing:
		run.Triggered = chgm.InvestigateTriggered
		run.Resolved = chgm.HandleResolved
	case investigation.ClusterProvisioningDelay:
		run.Triggered = cpd.InvestigateTriggered
	default:
		// Should never happen as GetAlertType should fail on unsupported alerts
		return errors.New("alert is not supported by CAD")
	}

	eventType := pdClient.GetEventType()
	logging.Infof("Starting investigation for %s with event type %s", alertTypeString, eventType)

	switch eventType {
	case pagerduty.IncidentTriggered:
		return run.Triggered(investigationResources)
	case pagerduty.IncidentResolved:
		return run.Resolved(investigationResources)
	case pagerduty.IncidentReopened:
		return run.Reopened(investigationResources)
	case pagerduty.IncidentEscalated:
		return run.Escalated(investigationResources)
	default:
		return fmt.Errorf("event type '%s' is not supported", eventType)
	}
}

// isAlertSupported will return the alertname as enum type of the alert if it is supported, otherwise an error
func isAlertSupported(alertTitle string) investigation.AlertType {
	// We currently map to the alert by using the title, we should use the name in the alert note in the future.
	// This currently isn't feasible yet, as CPD's alertmanager doesn't allow for the field to exist.

	// We can't switch case here as it's strings.Contains.
	if strings.Contains(alertTitle, "has gone missing") {
		return investigation.ClusterHasGoneMissing
	} else if strings.Contains(alertTitle, "ClusterProvisioningDelay -") {
		return investigation.ClusterProvisioningDelay
	}
	return investigation.Unsupported
}

// GetOCMClient will retrieve the OcmClient from the 'ocm' package
func GetOCMClient() (*ocm.SdkClient, error) {
	cadOcmFilePath := os.Getenv("CAD_OCM_FILE_PATH")

	_, err := os.Stat(cadOcmFilePath)
	if os.IsNotExist(err) {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		cadOcmFilePath = filepath.Join(configDir, "/ocm/ocm.json")
	}

	return ocm.New(cadOcmFilePath)
}

// GetPDClient will retrieve the PagerDuty from the 'pagerduty' package
func GetPDClient(webhookPayload []byte) (*pagerduty.SdkClient, error) {
	cadPD, hasCadPD := os.LookupEnv("CAD_PD_TOKEN")
	cadEscalationPolicy, hasCadEscalationPolicy := os.LookupEnv("CAD_ESCALATION_POLICY")
	cadSilentPolicy, hasCadSilentPolicy := os.LookupEnv("CAD_SILENT_POLICY")

	if !hasCadEscalationPolicy || !hasCadSilentPolicy || !hasCadPD {
		return nil, fmt.Errorf("one of the required envvars in the list '(CAD_ESCALATION_POLICY CAD_SILENT_POLICY CAD_PD_TOKEN)' is missing")
	}

	client, err := pagerduty.NewWithToken(cadEscalationPolicy, cadSilentPolicy, webhookPayload, cadPD)
	if err != nil {
		return nil, fmt.Errorf("could not initialize the client: %w", err)
	}

	return client, nil
}

func isAWS(cluster *cmv1.Cluster) (bool, error) {
	cloudProvider, ok := cluster.GetCloudProvider()
	if !ok {
		return false, fmt.Errorf("Failed to get clusters cloud provider")
	}

	return cloudProvider.ID() == "aws", nil
}

// Checks pre-requisites for a cluster investigation:
// - the cluster's state is supported by CAD for an investigation (= not uninstalling)
// - the cloud provider is supported by CAD (cluster is AWS)
// - the AWS account access flow is supported by CAD
// Performs according pagerduty actions and returns whether CAD needs to investigate the cluster
func clusterRequiresInvestigation(cluster *cmv1.Cluster, ocmClient *ocm.SdkClient, pdClient *pagerduty.SdkClient) (bool, error) {
	if cluster.State() == cmv1.ClusterStateUninstalling {
		logging.Info("Cluster is uninstalling and requires no investigation. Silencing alert.")
		return false, pdClient.SilenceAlertWithNote("CAD: Cluster is already uninstalling, silencing alert.")
	}

	isAWSCluster, err := isAWS(cluster)
	if err != nil {
		return false, err
	}

	if !isAWSCluster {
		return false, utils.EscalateAlertIfNotLS("CAD could not run an automated investigation on this cluster: unsupported cloud provider.", cluster, pdClient, ocmClient)
	}

	return true, nil
}
