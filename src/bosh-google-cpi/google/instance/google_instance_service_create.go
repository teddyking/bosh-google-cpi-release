package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	bosherr "github.com/cloudfoundry/bosh-utils/errors"

	"bosh-google-cpi/api"
	subnet "bosh-google-cpi/google/subnetwork"
	"bosh-google-cpi/util"

	"google.golang.org/api/compute/v1"
)

const defaultRootDiskSizeGb = 10
const userDataKey = "user_data"
const nodeGroupNodeAffinityKey = "compute.googleapis.com/node-group-name"

func (i GoogleInstanceService) Create(vmProps *Properties, networks Networks, registryEndpoint string) (string, error) {
	uuidStr, err := i.uuidGen.Generate()
	if err != nil {
		return "", bosherr.WrapErrorf(err, "Generating random Google Instance name")
	}

	instanceName := vmProps.Name
	if instanceName == "" {
		instanceName = fmt.Sprintf("%s-%s", googleInstanceNamePrefix, uuidStr)
	}
	canIPForward := networks.CanIPForward()
	diskParams := i.createDiskParams(vmProps.Stemcell, vmProps.RootDiskSizeGb, vmProps.RootDiskType)
	metadataParams, err := i.createMatadataParams(instanceName, registryEndpoint, networks)
	if err != nil {
		return "", err
	}
	networkInterfacesParams, err := i.createNetworkInterfacesParams(networks, vmProps.Zone)
	if err != nil {
		return "", err
	}
	schedulingParams := i.createSchedulingParams(vmProps.AutomaticRestart, vmProps.OnHostMaintenance, vmProps.Preemptible, vmProps.NodeGroup)
	serviceAccountsParams := i.createServiceAccountsParams(vmProps)

	// Handle tags
	allTags := append(networks.Tags(), vmProps.Tags...)
	tags := compute.Tags{}
	tags.Items = allTags.Unique()

	acceleratorParams := i.createAcceleratorParams(vmProps.Accelerators)

	var ssdDisk *compute.AttachedDisk

	if vmProps.EphemeralDiskType == "local-ssd" {
		// default is one local SSD
		numberOfLocalSSDs := 1

		// Parse MachineType to figure out how many CPUs the instance has.
		// Format: zones/zone/machineTypes/machine-type with machine-type being in the format
		// of either n1-standard-1, custom-4-5120, or a2-highgpu-1g
		machineTypeName := vmProps.MachineType[strings.LastIndex(vmProps.MachineType, "/")+1:]
		machineTypeComponents := strings.Split(machineTypeName, "-")
		machineTypeSeries := machineTypeComponents[0] // e.g. n1, custom, a2

		numberOfCPUs := 0

		if strings.HasPrefix(machineTypeSeries, "a2") {
			// a2 machine types (e.g. a2-highgpu-1g)
			gpuCount := machineTypeComponents[2]         // e.g. 1g
			gpuCount = strings.TrimSuffix(gpuCount, "g") // e.g. 1
			numberOfGPUs, err := strconv.Atoi(gpuCount)
			if err != nil {
				return "", err
			}
			// calculate number of CPUs based on number of GPUs per official documentation
			// https://cloud.google.com/compute/docs/accelerator-optimized-machines#a2-standard-vms
			numberOfCPUs = numberOfGPUs * 12
			if numberOfCPUs > 96 {
				numberOfCPUs = 96
			}
		} else if strings.HasPrefix(machineTypeSeries, "n") {
			// n* machine types (e.g. n1-standard-1)
			numberOfCPUs, err = strconv.Atoi(machineTypeComponents[2])
			if err != nil {
				return "", err
			}
		} else {
			// custom machine types (e.g. custom-4-5120)
			numberOfCPUs, err = strconv.Atoi(machineTypeComponents[1])
			if err != nil {
				return "", err
			}
		}

		// n2 and n2d series require a minimum number of SSDs depending on vCPUs
		// https://cloud.google.com/compute/docs/disks/local-ssd#lssd_disk_options
		if machineTypeSeries == "n2" { //nolint:staticcheck
			if numberOfCPUs >= 82 {
				numberOfLocalSSDs = 16
			} else if numberOfCPUs >= 42 {
				numberOfLocalSSDs = 8
			} else if numberOfCPUs >= 22 {
				numberOfLocalSSDs = 4
			} else if numberOfCPUs >= 12 {
				numberOfLocalSSDs = 2
			}
		} else if machineTypeSeries == "n2d" {
			if numberOfCPUs >= 96 {
				numberOfLocalSSDs = 8
			} else if numberOfCPUs >= 64 {
				numberOfLocalSSDs = 4
			} else if numberOfCPUs >= 32 {
				numberOfLocalSSDs = 2
			}
		}
		for j := 0; j < numberOfLocalSSDs; j++ {
			ssdDisk, err = i.createLocalSSDParams(vmProps.Zone, j+1)
			if err != nil {
				return "", err
			}

			diskParams = append(diskParams, ssdDisk)
		}
	}

	vm := &compute.Instance{
		Name:              instanceName,
		Description:       googleInstanceDescription,
		CanIpForward:      canIPForward,
		Disks:             diskParams,
		MachineType:       vmProps.MachineType,
		Metadata:          metadataParams,
		NetworkInterfaces: networkInterfacesParams,
		Scheduling:        schedulingParams,
		ServiceAccounts:   serviceAccountsParams,
		Tags:              &tags,
		Labels:            vmProps.Labels,
		GuestAccelerators: acceleratorParams,
		MinCpuPlatform:    "",
	}

	i.logger.Debug(googleInstanceServiceLogTag, "Creating Google Instance with params: %v", vm)
	operation, err := i.computeService.Instances.Insert(i.project, util.ResourceSplitter(vmProps.Zone), vm).Do()
	if err != nil {
		i.logger.Debug(googleInstanceServiceLogTag, "Failed to create Google Instance: %v", err)
		return "", api.NewVMCreationFailedError(err.Error(), true)
	}

	if operation, err = i.operationService.Waiter(operation, vmProps.Zone, ""); err != nil {
		i.logger.Debug(googleInstanceServiceLogTag, "Failed to create Google Instance: %v", err)
		i.CleanUp(vm.Name)
		return "", api.NewVMCreationFailedError(err.Error(), true)
	}

	if vmProps.TargetPool != "" {
		if err := i.addToTargetPool(operation.TargetLink, vmProps.TargetPool); err != nil {
			i.logger.Debug(googleInstanceServiceLogTag, "Failed to add created Google Instance to Target Pool: %v", err)
			i.CleanUp(vm.Name)
			return "", api.NewVMCreationFailedError(err.Error(), true)
		}
	}

	if &vmProps.BackendService != nil && vmProps.BackendService.Name != "" { //nolint:staticcheck
		if err := i.addToBackendService(operation.TargetLink, vmProps.BackendService); err != nil {
			i.logger.Debug(googleInstanceServiceLogTag, "Failed to add created Google Instance to Backend Service: %v", err)
			i.CleanUp(vm.Name)
			return "", api.NewVMCreationFailedError(err.Error(), true)
		}
	}

	return vm.Name, nil
}

func (i GoogleInstanceService) CleanUp(id string) {
	if err := i.Delete(id); err != nil {
		i.logger.Debug(googleInstanceServiceLogTag, "Failed cleaning up Google Instance '%s': %v", id, err)
	}

}

func (i GoogleInstanceService) createDiskParams(stemcell string, diskSize int, diskType string) []*compute.AttachedDisk {
	var disks []*compute.AttachedDisk

	if diskSize == 0 {
		diskSize = defaultRootDiskSizeGb
	}
	disk := &compute.AttachedDisk{
		AutoDelete: true,
		Boot:       true,
		InitializeParams: &compute.AttachedDiskInitializeParams{
			DiskSizeGb:  int64(diskSize),
			DiskType:    diskType,
			SourceImage: stemcell,
		},
		Mode: "READ_WRITE",
		Type: "PERSISTENT",
	}
	disks = append(disks, disk)

	return disks
}

func (i GoogleInstanceService) createLocalSSDParams(zone string, index int) (*compute.AttachedDisk, error) {
	diskType, b, e := i.diskTypeService.Find("local-ssd", zone)
	if e != nil {
		return nil, e
	}
	if !b {
		return nil, errors.New("disk not found")
	}

	disk := &compute.AttachedDisk{
		AutoDelete: true,
		Boot:       false,
		InitializeParams: &compute.AttachedDiskInitializeParams{
			DiskType: diskType.SelfLink,
		},
		Interface: "NVME",
		Index:     int64(index),
		Type:      "SCRATCH",
	}

	return disk, nil
}

func (i GoogleInstanceService) createAcceleratorParams(accelerators []Accelerator) []*compute.AcceleratorConfig {
	var accs []*compute.AcceleratorConfig

	for _, acc := range accelerators {
		accParam := &compute.AcceleratorConfig{
			AcceleratorType:  acc.AcceleratorType,
			AcceleratorCount: acc.Count,
		}
		accs = append(accs, accParam)
	}

	return accs
}

func (i GoogleInstanceService) createMatadataParams(name string, regEndpoint string, networks Networks) (*compute.Metadata, error) {
	serverName := GoogleUserDataServerName{Name: name}
	registryEndpoint := GoogleUserDataRegistryEndpoint{Endpoint: regEndpoint}
	userData := GoogleUserData{Server: serverName, Registry: registryEndpoint}

	if networkDNS := networks.DNS(); len(networkDNS) > 0 {
		userData.DNS = GoogleUserDataDNSItems{NameServer: networkDNS}
	}

	ud, err := json.Marshal(userData)
	if err != nil {
		return nil, bosherr.WrapErrorf(err, "Marshalling user data")
	}

	var metadataItems []*compute.MetadataItems
	userDataValue := string(ud)
	metadataItem := &compute.MetadataItems{Key: userDataKey, Value: &userDataValue}
	metadataItems = append(metadataItems, metadataItem)
	metadata := &compute.Metadata{Items: metadataItems}

	return metadata, nil
}

func (i GoogleInstanceService) createNetworkInterfacesParams(networks Networks, zone string) ([]*compute.NetworkInterface, error) {
	network, found, err := i.networkService.Find(networks.NetworkProjectID(), networks.NetworkName())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, bosherr.WrapErrorf(err, "Network '%s' does not exist in project '%s'", networks.NetworkName(), networks.NetworkProjectID())
	}

	subnetworkLink := ""
	if networks.SubnetworkName() != "" {
		subnetwork, err := i.subnetworkService.Find(networks.NetworkProjectID(), networks.SubnetworkName(), util.RegionFromZone(zone))
		if err != nil {
			if err == subnet.ErrSubnetNotFound {
				return nil, bosherr.WrapErrorf(err, "Subnetwork '%s' does not exist in project '%s'", networks.SubnetworkName(), networks.NetworkProjectID())
			}
			return nil, err
		}
		subnetworkLink = subnetwork.SelfLink
	}

	var networkInterfaces []*compute.NetworkInterface
	var accessConfigs []*compute.AccessConfig

	vipNetwork := networks.VipNetwork()
	if networks.EphemeralExternalIP() || vipNetwork.IP != "" {
		accessConfig := &compute.AccessConfig{
			Name: "External NAT",
			Type: "ONE_TO_ONE_NAT",
		}
		if vipNetwork.IP != "" {
			accessConfig.NatIP = vipNetwork.IP
		}
		accessConfigs = append(accessConfigs, accessConfig)
	}

	networkInterface := &compute.NetworkInterface{
		Network:       network.SelfLink,
		Subnetwork:    subnetworkLink,
		AccessConfigs: accessConfigs,
		NetworkIP:     networks.StaticPrivateIP(),
	}
	networkInterfaces = append(networkInterfaces, networkInterface)

	return networkInterfaces, nil
}

func (i GoogleInstanceService) createSchedulingParams(
	automaticRestart bool,
	onHostMaintenance string,
	preemptible bool,
	nodeGroup string,
) *compute.Scheduling {
	if preemptible {
		return &compute.Scheduling{Preemptible: preemptible}
	}

	scheduling := &compute.Scheduling{
		AutomaticRestart:  &automaticRestart,
		OnHostMaintenance: onHostMaintenance,
		Preemptible:       preemptible,
	}

	if nodeGroup != "" {
		scheduling.NodeAffinities = []*compute.SchedulingNodeAffinity{{
			Key:      nodeGroupNodeAffinityKey,
			Operator: "IN",
			Values:   []string{nodeGroup},
		}}
	}

	if onHostMaintenance == "" {
		scheduling.OnHostMaintenance = "MIGRATE"
	}

	return scheduling
}

func (i GoogleInstanceService) createServiceAccountsParams(vmProps *Properties) []*compute.ServiceAccount {
	// No service account and no scopes, so return an empty slice.
	if vmProps.ServiceAccount == "" && len(vmProps.ServiceScopes) == 0 {
		return nil
	}

	// No service account, but scopes are specified. Set the "default" account.
	if vmProps.ServiceAccount == "" && len(vmProps.ServiceScopes) > 0 {
		vmProps.ServiceAccount = "default"
	}

	// A service account, but no scopes. Set the "full access" scope.
	if vmProps.ServiceAccount != "" && len(vmProps.ServiceScopes) == 0 {
		vmProps.ServiceScopes = ServiceScopes([]string{"https://www.googleapis.com/auth/cloud-platform"})
	}

	// Format scopes and create a slice of *compute.ServiceAccount
	var scopes []string
	for _, scope := range vmProps.ServiceScopes {
		if strings.HasPrefix(scope, "https://www.googleapis.com/auth/") {
			scopes = append(scopes, scope)
		} else {
			scopes = append(scopes, fmt.Sprintf("https://www.googleapis.com/auth/%s", scope))
		}
	}

	serviceAccount := &compute.ServiceAccount{
		Email:  string(vmProps.ServiceAccount),
		Scopes: scopes,
	}
	return []*compute.ServiceAccount{serviceAccount}
}

func (i GoogleInstanceService) addToTargetPool(instanceSelfLink string, targetPoolName string) error {
	if err := i.targetPoolService.AddInstance(targetPoolName, instanceSelfLink); err != nil {
		return err
	}

	return nil
}

func (i GoogleInstanceService) removeFromTargetPool(instanceSelfLink string) error {
	targetPool, found, err := i.targetPoolService.FindByInstance(instanceSelfLink, "")
	if err != nil {
		return err
	}

	if found {
		if err := i.targetPoolService.RemoveInstance(targetPool, instanceSelfLink); err != nil {
			return err
		}
	}

	return nil
}

func (i GoogleInstanceService) addToBackendService(instanceSelfLink string, backendService BackendService) error {
	if err := i.backendServiceService.AddInstance(backendService.Name, instanceSelfLink); err != nil {
		return err
	}

	return nil
}

func (i GoogleInstanceService) removeFromBackendService(instanceSelfLink string) error {
	if err := i.backendServiceService.RemoveInstance(instanceSelfLink); err != nil {
		return err
	}

	return nil
}
