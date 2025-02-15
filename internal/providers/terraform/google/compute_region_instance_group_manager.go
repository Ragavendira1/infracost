package google

import (
	"github.com/infracost/infracost/internal/resources/google"
	"github.com/infracost/infracost/internal/schema"
)

func getComputeRegionInstanceGroupManagerRegistryItem() *schema.RegistryItem {
	return &schema.RegistryItem{
		Name:                "google_compute_region_instance_group_manager",
		RFunc:               newComputeRegionInstanceGroupManager,
		Notes:               []string{"Multiple versions are not supported."},
		ReferenceAttributes: []string{"version.0.instance_template"},
	}
}

func newComputeRegionInstanceGroupManager(d *schema.ResourceData, u *schema.UsageData) *schema.Resource {
	region := d.Get("region").String()

	targetSize := int64(1)
	if d.Get("target_size").Exists() {
		targetSize = d.Get("target_size").Int()
	}

	var machineType string
	purchaseOption := "on_demand"
	disks := []*google.ComputeDisk{}
	guestAccelerators := []*google.ComputeGuestAccelerator{}

	if len(d.References("version.0.instance_template")) > 0 {
		instanceTemplate := d.References("version.0.instance_template")[0]

		machineType = instanceTemplate.Get("machine_type").String()

		if instanceTemplate.Get("scheduling.0.preemptible").Bool() {
			purchaseOption = "preemptible"
		}

		for _, disk := range instanceTemplate.Get("disk").Array() {
			diskSize := int64(100)
			if size := disk.Get("disk_size_gb"); size.Exists() {
				diskSize = size.Int()
			}
			diskType := disk.Get("disk_type").String()

			disks = append(disks, &google.ComputeDisk{
				Type: diskType,
				Size: float64(diskSize),
			})
		}

		guestAccelerators = collectComputeGuestAccelerators(instanceTemplate.Get("guest_accelerator"))
	}

	r := &google.ComputeRegionInstanceGroupManager{
		Address:           d.Address,
		Region:            region,
		MachineType:       machineType,
		PurchaseOption:    purchaseOption,
		TargetSize:        targetSize,
		Disks:             disks,
		GuestAccelerators: guestAccelerators,
	}
	r.PopulateUsage(u)

	return r.BuildResource()
}
