package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-03-01/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	snapv1 "github.com/kubernetes-incubator/external-storage/snapshot/pkg/apis/crd/v1"
	snapshotVolume "github.com/kubernetes-incubator/external-storage/snapshot/pkg/volume"
	storkvolume "github.com/libopenstorage/stork/drivers/volume"
	storkapi "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	"github.com/libopenstorage/stork/pkg/errors"
	"github.com/libopenstorage/stork/pkg/log"
	"github.com/portworx/sched-ops/k8s"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	k8shelper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
)

const (
	// driverName is the name of the azure driver implementation
	driverName = "azure"
	// provisioner names for azure disks
	provisionerName = "kubernetes.io/azure-disk"
	// pvcProvisionerAnnotation is the annotation on PVC which has the
	// provisioner name
	pvcProvisionerAnnotation = "volume.beta.kubernetes.io/storage-provisioner"
	// pvProvisionedByAnnotation is the annotation on PV which has the
	// provisioner name
	pvProvisionedByAnnotation = "pv.kubernetes.io/provisioned-by"
	pvNamePrefix              = "pvc-"
	subscriptionIDKey         = "subscriptionId"
	resourceGroupKey          = "resourceGroupName"
	metadataURL               = "http://169.254.169.254/metadata/instance/compute"
	apiVersion                = "2018-02-01"
)

type azure struct {
	resourceGroup  string
	diskClient     compute.DisksClient
	snapshotClient compute.SnapshotsClient
	storkvolume.ClusterPairNotSupported
	storkvolume.MigrationNotSupported
	storkvolume.GroupSnapshotNotSupported
	storkvolume.ClusterDomainsNotSupported
	storkvolume.CloneNotSupported
	storkvolume.SnapshotRestoreNotSupported
}

func (a *azure) Init(_ interface{}) error {

	authorizer, err := auth.NewAuthorizerFromEnvironment()
	if err != nil {
		return err
	}
	metadata, err := a.getMetadata()
	if err != nil {
		return err
	}
	var ok bool
	var subscriptionID string
	if subscriptionID, ok = metadata[subscriptionIDKey]; !ok {
		return fmt.Errorf("error detecting subscription ID from cluster context")
	}

	a.diskClient = compute.NewDisksClient(subscriptionID)
	a.snapshotClient = compute.NewSnapshotsClient(subscriptionID)
	a.diskClient.Authorizer = authorizer
	a.snapshotClient.Authorizer = authorizer

	if a.resourceGroup, ok = metadata[resourceGroupKey]; !ok {
		return fmt.Errorf("error detecting subscription ID from cluster context")
	}

	return nil
}

func (a *azure) getMetadata() (map[string]string, error) {
	client := http.Client{Timeout: time.Second * 3}

	req, err := http.NewRequest("GET", metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error querying Azure metadata: %v", err)
	}

	req.Header.Add("Metadata", "True")

	q := req.URL.Query()
	q.Add("format", "json")
	q.Add("api-version", apiVersion)
	req.URL.RawQuery = q.Encode()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error querying Azure metadata: %v", err)
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			logrus.Errorf("Error closing body when getching azure metadate: %v", err)
		}
	}()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("error querying Azure metadata: Code %d returned for url %s", resp.StatusCode, req.URL)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error querying Azure metadata: %v", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("error querying Azure metadata: Empty response")
	}

	metadata := make(map[string]string)
	err = json.Unmarshal(body, &metadata)
	if err != nil {
		return nil, fmt.Errorf("error parsing Azure metadata: %v", err)
	}

	return metadata, nil
}

func (a *azure) String() string {
	return driverName
}

func (a *azure) Stop() error {
	return nil
}

func (a *azure) OwnsPVC(pvc *v1.PersistentVolumeClaim) bool {

	provisioner := ""
	// Check for the provisioner in the PVC annotation. If not populated
	// try getting the provisioner from the Storage class.
	if val, ok := pvc.Annotations[pvcProvisionerAnnotation]; ok {
		provisioner = val
	} else {
		storageClassName := k8shelper.GetPersistentVolumeClaimClass(pvc)
		if storageClassName != "" {
			storageClass, err := k8s.Instance().GetStorageClass(storageClassName)
			if err == nil {
				provisioner = storageClass.Provisioner
			} else {
				logrus.Warnf("Error getting storageclass %v for pvc %v: %v", storageClassName, pvc.Name, err)
			}
		}
	}

	if provisioner == "" {
		// Try to get info from the PV since storage class could be deleted
		pv, err := k8s.Instance().GetPersistentVolume(pvc.Spec.VolumeName)
		if err != nil {
			logrus.Warnf("Error getting pv %v for pvc %v: %v", pvc.Spec.VolumeName, pvc.Name, err)
			return false
		}
		return a.OwnsPV(pv)
	}

	if provisioner != provisionerName &&
		!isCsiProvisioner(provisioner) {
		logrus.Debugf("Provisioner in Storageclass not Azure: %v", provisioner)
		return false
	}
	return true
}

func (a *azure) OwnsPV(pv *v1.PersistentVolume) bool {
	var provisioner string
	// Check the annotation in the PV for the provisioner
	if val, ok := pv.Annotations[pvProvisionedByAnnotation]; ok {
		provisioner = val
	} else {
		// Finally check the volume reference in the spec
		if pv.Spec.AzureDisk != nil {
			return true
		}
	}
	if provisioner != provisionerName &&
		!isCsiProvisioner(provisioner) {
		logrus.Debugf("Provisioner in Storageclass not AzureDisk: %v", provisioner)
		return false
	}
	return true
}

func isCsiProvisioner(provisioner string) bool {
	return false
}

func (a *azure) StartBackup(backup *storkapi.ApplicationBackup,
	pvcs []v1.PersistentVolumeClaim,
) ([]*storkapi.ApplicationBackupVolumeInfo, error) {
	volumeInfos := make([]*storkapi.ApplicationBackupVolumeInfo, 0)

	for _, pvc := range pvcs {
		if pvc.DeletionTimestamp != nil {
			log.ApplicationBackupLog(backup).Warnf("Ignoring PVC %v which is being deleted", pvc.Name)
			continue
		}
		volumeInfo := &storkapi.ApplicationBackupVolumeInfo{}
		volumeInfo.PersistentVolumeClaim = pvc.Name
		volumeInfo.Namespace = pvc.Namespace
		volumeInfo.DriverName = driverName
		volumeInfo.Options = map[string]string{
			resourceGroupKey: a.resourceGroup,
		}
		volumeInfos = append(volumeInfos, volumeInfo)

		pvName, err := k8s.Instance().GetVolumeForPersistentVolumeClaim(&pvc)
		if err != nil {
			return nil, fmt.Errorf("error getting PV name for PVC (%v/%v): %v", pvc.Namespace, pvc.Name, err)
		}
		pv, err := k8s.Instance().GetPersistentVolume(pvName)
		if err != nil {
			return nil, fmt.Errorf("error getting pv %v: %v", pvName, err)
		}
		volume := pv.Spec.AzureDisk.DiskName
		disk, err := a.diskClient.Get(context.TODO(), a.resourceGroup, volume)
		if err != nil {
			return nil, err
		}
		volumeInfo.Volume = pvc.Spec.VolumeName
		snapshot := compute.Snapshot{
			Name: to.StringPtr("stork-snapshot-" + string(uuid.NewUUID())),
			SnapshotProperties: &compute.SnapshotProperties{
				CreationData: &compute.CreationData{
					CreateOption:     compute.Copy,
					SourceResourceID: disk.ID,
				},
			},
			Tags: map[string]*string{
				"created-by":           to.StringPtr("stork"),
				"backup-uid":           to.StringPtr(string(backup.UID)),
				"source-pvc-name":      to.StringPtr(pvc.Name),
				"source-pvc-namespace": to.StringPtr(pvc.Namespace),
			},
			Location: disk.Location,
		}
		_, err = a.snapshotClient.CreateOrUpdate(context.TODO(), a.resourceGroup, *snapshot.Name, snapshot)
		if err != nil {
			return nil, fmt.Errorf("error triggering backup for volume: %v (PVC: %v, Namespace: %v): %v", volume, pvc.Name, pvc.Namespace, err)
		}
		volumeInfo.BackupID = *snapshot.Name
	}
	return volumeInfos, nil
}

func (a *azure) GetBackupStatus(backup *storkapi.ApplicationBackup) ([]*storkapi.ApplicationBackupVolumeInfo, error) {
	volumeInfos := make([]*storkapi.ApplicationBackupVolumeInfo, 0)

	for _, vInfo := range backup.Status.Volumes {
		if vInfo.DriverName != driverName {
			continue
		}
		snapshot, err := a.snapshotClient.Get(context.TODO(), a.resourceGroup, vInfo.BackupID)
		if err != nil {
			return nil, err
		}
		switch *snapshot.ProvisioningState {
		case "Failed":
			vInfo.Status = storkapi.ApplicationBackupStatusFailed
			vInfo.Reason = fmt.Sprintf("Backup failed for volume: %v", snapshot.ProvisioningState)
		case "Succeeded":
			vInfo.Status = storkapi.ApplicationBackupStatusSuccessful
			vInfo.Reason = "Backup successful for volume"
		default:
			vInfo.Status = storkapi.ApplicationBackupStatusInProgress
			vInfo.Reason = fmt.Sprintf("Volume backup in progress: %v", snapshot.ProvisioningState)
		}
		volumeInfos = append(volumeInfos, vInfo)
	}

	return volumeInfos, nil

}

func (a *azure) CancelBackup(backup *storkapi.ApplicationBackup) error {
	return a.DeleteBackup(backup)
}

func (a *azure) DeleteBackup(backup *storkapi.ApplicationBackup) error {
	for _, vInfo := range backup.Status.Volumes {
		if vInfo.DriverName != driverName {
			continue
		}
		_, err := a.snapshotClient.Delete(context.TODO(), a.resourceGroup, vInfo.BackupID)
		if err != nil {
			// Ignore if the snaphot has already been deleted
			if azureErr, ok := err.(autorest.DetailedError); ok {
				if azureErr.StatusCode == http.StatusNotFound {
					continue
				}
			}
		}
	}
	return nil
}

func (a *azure) UpdateMigratedPersistentVolumeSpec(
	pv *v1.PersistentVolume,
) (*v1.PersistentVolume, error) {
	if pv.Spec.CSI != nil {
		pv.Spec.CSI.VolumeHandle = pv.Name
		return pv, nil
	}

	pv.Spec.AzureDisk.DiskName = pv.Name
	disk, err := a.diskClient.Get(context.TODO(), a.resourceGroup, pv.Name)
	if err != nil {
		return nil, err
	}
	pv.Spec.AzureDisk.DataDiskURI = *disk.ID

	return pv, nil
}

func (a *azure) generatePVName() string {
	return pvNamePrefix + string(uuid.NewUUID())
}

func (a *azure) StartRestore(
	restore *storkapi.ApplicationRestore,
	volumeBackupInfos []*storkapi.ApplicationBackupVolumeInfo,
) ([]*storkapi.ApplicationRestoreVolumeInfo, error) {

	volumeInfos := make([]*storkapi.ApplicationRestoreVolumeInfo, 0)
	for _, backupVolumeInfo := range volumeBackupInfos {
		var resourceGroup string
		if val, present := backupVolumeInfo.Options[resourceGroupKey]; present {
			resourceGroup = val
		} else {
			resourceGroup = a.resourceGroup
			logrus.Warnf("missing resource group in snapshot %v, will use current resource group", backupVolumeInfo.BackupID)
		}

		snapshot, err := a.snapshotClient.Get(context.TODO(), resourceGroup, backupVolumeInfo.BackupID)
		if err != nil {
			return nil, err
		}
		volumeInfo := &storkapi.ApplicationRestoreVolumeInfo{}
		volumeInfo.PersistentVolumeClaim = backupVolumeInfo.PersistentVolumeClaim
		volumeInfo.SourceNamespace = backupVolumeInfo.Namespace
		volumeInfo.SourceVolume = backupVolumeInfo.Volume
		volumeInfo.RestoreVolume = a.generatePVName()
		volumeInfo.DriverName = driverName
		volumeInfos = append(volumeInfos, volumeInfo)
		disk := compute.Disk{

			Name: &volumeInfo.RestoreVolume,
			DiskProperties: &compute.DiskProperties{
				CreationData: &compute.CreationData{
					CreateOption:     compute.Copy,
					SourceResourceID: snapshot.ID,
				},
			},
			Tags: map[string]*string{
				"created-by":           to.StringPtr("stork"),
				"restore-uid":          to.StringPtr(string(restore.UID)),
				"source-pvc-name":      to.StringPtr(volumeInfo.PersistentVolumeClaim),
				"source-pvc-namespace": to.StringPtr(volumeInfo.SourceNamespace),
			},
			Location: snapshot.Location,
		}
		_, err = a.diskClient.CreateOrUpdate(context.TODO(), a.resourceGroup, *disk.Name, disk)
		if err != nil {
			return nil, fmt.Errorf("error triggering restore for volume: %v: %v",
				backupVolumeInfo.Volume, err)
		}
	}
	return volumeInfos, nil
}

func (a *azure) CancelRestore(*storkapi.ApplicationRestore) error {
	// Do nothing to cancel restores for now
	return nil
}

func (a *azure) GetRestoreStatus(restore *storkapi.ApplicationRestore) ([]*storkapi.ApplicationRestoreVolumeInfo, error) {
	volumeInfos := make([]*storkapi.ApplicationRestoreVolumeInfo, 0)
	for _, vInfo := range restore.Status.Volumes {
		disk, err := a.diskClient.Get(context.TODO(), a.resourceGroup, vInfo.RestoreVolume)
		if err != nil {
			return nil, err
		}
		switch *disk.ProvisioningState {
		case "Failed":
			vInfo.Status = storkapi.ApplicationRestoreStatusFailed
			vInfo.Reason = fmt.Sprintf("Restore failed for volume: %v", disk.ProvisioningState)
		case "Succeeded":
			vInfo.Status = storkapi.ApplicationRestoreStatusSuccessful
			vInfo.Reason = "Restore successful for volume"
		default:
			vInfo.Status = storkapi.ApplicationRestoreStatusInProgress
			vInfo.Reason = fmt.Sprintf("Volume restore in progress: %v", disk.ProvisioningState)
		}
		volumeInfos = append(volumeInfos, vInfo)
	}

	return volumeInfos, nil
}

func (a *azure) InspectVolume(volumeID string) (*storkvolume.Info, error) {
	return nil, &errors.ErrNotSupported{}
}

func (a *azure) GetClusterID() (string, error) {
	return "", &errors.ErrNotSupported{}
}

func (a *azure) GetNodes() ([]*storkvolume.NodeInfo, error) {
	return nil, &errors.ErrNotSupported{}
}

func (a *azure) GetPodVolumes(podSpec *v1.PodSpec, namespace string) ([]*storkvolume.Info, error) {
	return nil, &errors.ErrNotSupported{}
}

func (a *azure) GetSnapshotPlugin() snapshotVolume.Plugin {
	return nil
}

func (a *azure) GetSnapshotType(snap *snapv1.VolumeSnapshot) (string, error) {
	return "", &errors.ErrNotSupported{}
}

func (a *azure) GetVolumeClaimTemplates([]v1.PersistentVolumeClaim) (
	[]v1.PersistentVolumeClaim, error) {
	return nil, &errors.ErrNotSupported{}
}

func init() {
	a := &azure{}
	err := a.Init(nil)
	if err != nil {
		logrus.Debugf("Error init'ing azure driver: %v", err)
		return
	}
	if err := storkvolume.Register(driverName, a); err != nil {
		logrus.Panicf("Error registering azure volume driver: %v", err)
	}
}
