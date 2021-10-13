package compute

import (
	"fmt"
	"log"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/hashicorp/terraform-provider-azurerm/helpers/azure"
	"github.com/hashicorp/terraform-provider-azurerm/helpers/tf"
	"github.com/hashicorp/terraform-provider-azurerm/helpers/validate"
	"github.com/hashicorp/terraform-provider-azurerm/internal/clients"
	"github.com/hashicorp/terraform-provider-azurerm/internal/location"
	"github.com/hashicorp/terraform-provider-azurerm/internal/services/compute/parse"
	computeValidate "github.com/hashicorp/terraform-provider-azurerm/internal/services/compute/validate"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tags"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tf/pluginsdk"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tf/suppress"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tf/validation"
	"github.com/hashicorp/terraform-provider-azurerm/internal/timeouts"
	"github.com/hashicorp/terraform-provider-azurerm/utils"
)

func resourceOrchestratedVirtualMachineScaleSet() *pluginsdk.Resource {
	return &pluginsdk.Resource{
		Create: resourceOrchestratedVirtualMachineScaleSetCreate,
		Read:   resourceOrchestratedVirtualMachineScaleSetRead,
		Update: resourceOrchestratedVirtualMachineScaleSetUpdate,
		Delete: resourceOrchestratedVirtualMachineScaleSetDelete,

		Importer: pluginsdk.ImporterValidatingResourceIdThen(func(id string) error {
			_, err := parse.VirtualMachineScaleSetID(id)
			return err
		}, importOrchestratedVirtualMachineScaleSet),

		Timeouts: &pluginsdk.ResourceTimeout{
			Create: pluginsdk.DefaultTimeout(60 * time.Minute),
			Read:   pluginsdk.DefaultTimeout(5 * time.Minute),
			Update: pluginsdk.DefaultTimeout(60 * time.Minute),
			Delete: pluginsdk.DefaultTimeout(60 * time.Minute),
		},

		// TODO: exposing requireGuestProvisionSignal once it's available
		// https://github.com/Azure/azure-rest-api-specs/pull/7246

		Schema: map[string]*pluginsdk.Schema{
			"name": {
				Type:         pluginsdk.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: computeValidate.VirtualMachineName,
			},

			"resource_group_name": azure.SchemaResourceGroupName(),

			"location": azure.SchemaLocation(),

			"network_interface": OrchestratedVirtualMachineScaleSetNetworkInterfaceSchema(),

			"os_disk": OrchestratedVirtualMachineScaleSetOSDiskSchema(),

			// For sku I will create a format like: tier_sku name_capacity. Capacity can be from 0 to 1000
			// NOTE: all of the exposed vm sku tier's are Standard so this will continue to be hardcoded
			// Examples: Standard_HC44rs_4, Standard_D48_v3_6, Standard_M64s_20, Standard_HB120-96rs_v3_8
			"sku_name": {
				Type:         pluginsdk.TypeString,
				Optional:     true,
				ValidateFunc: azure.ValidateOrchestratedVirtualMachineScaleSetSku,
			},

			"os_profile": OrchestratedVirtualMachineScaleSetOSProfileSchema(),

			// Optional
			"automatic_instance_repair": OrchestratedVirtualMachineScaleSetAutomaticRepairsPolicySchema(),

			"boot_diagnostics": bootDiagnosticsSchema(),

			"data_disk": OrchestratedVirtualMachineScaleSetDataDiskSchema(),

			"encryption_at_host_enabled": {
				Type:     pluginsdk.TypeBool,
				Optional: true,
			},

			"eviction_policy": {
				// only applicable when `priority` is set to `Spot`
				Type:     pluginsdk.TypeString,
				Optional: true,
				ForceNew: true,
				ValidateFunc: validation.StringInSlice([]string{
					string(compute.VirtualMachineEvictionPolicyTypesDeallocate),
					string(compute.VirtualMachineEvictionPolicyTypesDelete),
				}, false),
			},

			"extension": OrchestratedVirtualMachineScaleSetExtensionsSchema(),

			"extensions_time_budget": {
				Type:         pluginsdk.TypeString,
				Optional:     true,
				Default:      "PT1H30M",
				ValidateFunc: validate.ISO8601DurationBetween("PT15M", "PT2H"),
			},

			"identity": OrchestratedVirtualMachineScaleSetIdentitySchema(),

			"license_type": {
				Type:     pluginsdk.TypeString,
				Optional: true,
				ValidateFunc: validation.StringInSlice([]string{
					"None",
					"Windows_Client",
					"Windows_Server",
				}, false),
				DiffSuppressFunc: func(_, old, new string, _ *pluginsdk.ResourceData) bool {
					if old == "None" && new == "" || old == "" && new == "None" {
						return true
					}

					return false
				},
			},

			"max_bid_price": {
				Type:         pluginsdk.TypeFloat,
				Optional:     true,
				Default:      -1,
				ValidateFunc: computeValidate.SpotMaxPrice,
			},

			"plan": planSchema(),

			"platform_fault_domain_count": {
				Type:     pluginsdk.TypeInt,
				Required: true,
				ForceNew: true,
			},

			"priority": {
				Type:     pluginsdk.TypeString,
				Optional: true,
				ForceNew: true,
				Default:  string(compute.VirtualMachinePriorityTypesRegular),
				ValidateFunc: validation.StringInSlice([]string{
					string(compute.VirtualMachinePriorityTypesRegular),
					string(compute.VirtualMachinePriorityTypesSpot),
				}, false),
			},

			"proximity_placement_group_id": {
				Type:         pluginsdk.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: azure.ValidateResourceID,
				// the Compute API is broken and returns the Resource Group name in UPPERCASE :shrug:, github issue: https://github.com/Azure/azure-rest-api-specs/issues/10016
				DiffSuppressFunc: suppress.CaseDifference,
			},

			// removing single_placement_group since it has been retired as of version 2019-12-01 for Flex VMSS
			"source_image_id": {
				Type:         pluginsdk.TypeString,
				Optional:     true,
				ValidateFunc: azure.ValidateResourceID,
			},

			"source_image_reference": sourceImageReferenceSchema(false),

			"zone_balance": {
				Type:     pluginsdk.TypeBool,
				Optional: true,
				ForceNew: true,
				Default:  false,
			},

			"terminate_notification": OrchestratedVirtualMachineScaleSetTerminateNotificationSchema(),

			"zones": azure.SchemaZones(),

			"tags": tags.Schema(),

			// Computed
			"unique_id": {
				Type:     pluginsdk.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceOrchestratedVirtualMachineScaleSetCreate(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Compute.VMScaleSetClient
	ctx, cancel := timeouts.ForCreate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	isLegacy := true
	resourceGroup := d.Get("resource_group_name").(string)
	name := d.Get("name").(string)

	if d.IsNewResource() {
		// Upgrading to the 2021-07-01 exposed a new expand parameter to the GET method
		existing, err := client.Get(ctx, resourceGroup, name, compute.ExpandTypesForGetVMScaleSetsUserData)
		if err != nil {
			if !utils.ResponseWasNotFound(existing.Response) {
				return fmt.Errorf("checking for existing Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", name, resourceGroup, err)
			}
		}

		if existing.ID != nil && *existing.ID != "" {
			return tf.ImportAsExistsError("azurerm_orchestrated_virtual_machine_scale_set", *existing.ID)
		}
	}

	location := azure.NormalizeLocation(d.Get("location").(string))
	t := d.Get("tags").(map[string]interface{})
	zones := azure.ExpandZones(d.Get("zones").([]interface{}))

	props := compute.VirtualMachineScaleSet{
		Location: utils.String(location),
		Tags:     tags.Expand(t),
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
			PlatformFaultDomainCount: utils.Int32(int32(d.Get("platform_fault_domain_count").(int))),
			SinglePlacementGroup:     utils.Bool(false),
			// OrchestrationMode needs to be hardcoded to Uniform, for the
			// standard VMSS resource, since virtualMachineProfile is now supported
			// in both VMSS and Orchestrated VMSS...
			OrchestrationMode: compute.OrchestrationModeFlexible,
		},
		Zones: zones,
	}

	virtualMachineProfile := compute.VirtualMachineScaleSetVMProfile{
		StorageProfile: &compute.VirtualMachineScaleSetStorageProfile{},
	}

	networkProfile := &compute.VirtualMachineScaleSetNetworkProfile{
		// 2020-11-01 is the only valid value for this value and is only valid for VMSS in Orchestration Mode flex
		NetworkAPIVersion: compute.NetworkAPIVersionTwoZeroTwoZeroHyphenMinusOneOneHyphenMinusZeroOne,
	}

	if v, ok := d.GetOk("proximity_placement_group_id"); ok {
		props.VirtualMachineScaleSetProperties.ProximityPlacementGroup = &compute.SubResource{
			ID: utils.String(v.(string)),
		}
	}

	if v, ok := d.GetOk("sku_name"); ok {
		isLegacy = false
		sku, err := azure.ExpandOrchestratedVirtualMachineScaleSetSku(v.(string))
		if err != nil {
			return fmt.Errorf("expanding 'sku_name': %+v", err)
		}
		props.Sku = sku
	}

	osType := compute.OperatingSystemTypesWindows
	var winConfigRaw []interface{}
	var linConfigRaw []interface{}
	var vmssOsProfile *compute.VirtualMachineScaleSetOSProfile
	osProfileRaw := d.Get("os_profile").([]interface{})

	if len(osProfileRaw) > 0 {
		osProfile := osProfileRaw[0].(map[string]interface{})
		winConfigRaw = osProfile["windows_configuration"].([]interface{})
		linConfigRaw = osProfile["linux_configuration"].([]interface{})
		customData := ""

		// Pass custom data if it is defined in the config file
		if v := osProfile["custom_data"]; v != nil {
			customData = v.(string)
		}

		if len(winConfigRaw) > 0 {
			winConfig := winConfigRaw[0].(map[string]interface{})
			vmssOsProfile = expandOrchestratedVirtualMachineScaleSetOsProfileWithWindowsConfiguration(winConfig, customData)

			// if the Computer Prefix Name was not defined use the computer name
			if vmssOsProfile.ComputerNamePrefix == nil || len(*vmssOsProfile.ComputerNamePrefix) == 0 {
				// validate that the computer name is a valid Computer Prefix Name
				_, errs := computeValidate.WindowsComputerNamePrefix(name, "computer_name_prefix")
				if len(errs) > 0 {
					return fmt.Errorf("unable to assume default computer name prefix %s. Please adjust the %q, or specify an explicit %q", errs[0], "name", "computer_name_prefix")
				}
				vmssOsProfile.ComputerNamePrefix = utils.String(name)
			}
		}

		if len(linConfigRaw) > 0 {
			osType = compute.OperatingSystemTypesLinux
			linConfig := linConfigRaw[0].(map[string]interface{})
			vmssOsProfile = expandOrchestratedVirtualMachineScaleSetOsProfileWithLinuxConfiguration(linConfig, customData)

			// if the Computer Prefix Name was not defined use the computer name
			if len(*vmssOsProfile.ComputerNamePrefix) == 0 {
				// validate that the computer name is a valid Computer Prefix Name
				_, errs := computeValidate.LinuxComputerNamePrefix(name, "computer_name_prefix")
				if len(errs) > 0 {
					return fmt.Errorf("unable to assume default computer name prefix %s. Please adjust the %q, or specify an explicit %q", errs[0], "name", "computer_name_prefix")
				}
				vmssOsProfile.ComputerNamePrefix = utils.String(name)
			}
		}

		virtualMachineProfile.OsProfile = vmssOsProfile
	}

	if v, ok := d.GetOk("boot_diagnostics"); ok {
		virtualMachineProfile.DiagnosticsProfile = expandBootDiagnostics(v.([]interface{}))
	}

	if v, ok := d.GetOk("priority"); ok {
		virtualMachineProfile.Priority = compute.VirtualMachinePriorityTypes(v.(string))
	}

	if v, ok := d.GetOk("os_disk"); ok {
		virtualMachineProfile.StorageProfile.OsDisk = ExpandOrchestratedVirtualMachineScaleSetOSDisk(v.([]interface{}), osType)
	}

	if v, ok := d.GetOk("source_image_reference"); ok {
		sourceImageId := ""
		if sid, ok := d.GetOk("source_image_id"); ok {
			sourceImageId = sid.(string)
		}
		sourceImageReference, err := expandSourceImageReference(v.([]interface{}), sourceImageId)
		if err != nil {
			return err
		}
		virtualMachineProfile.StorageProfile.ImageReference = sourceImageReference
	}

	if v, ok := d.GetOk("data_disk"); ok {
		ultraSSDEnabled := false // Currently not supported in orchestrated VMSS
		dataDisks, err := ExpandVirtualMachineScaleSetDataDisk(v.([]interface{}), ultraSSDEnabled)
		if err != nil {
			return fmt.Errorf("expanding `data_disk`: %+v", err)
		}
		virtualMachineProfile.StorageProfile.DataDisks = dataDisks
	}

	if v, ok := d.GetOk("network_interface"); ok {
		networkInterfaces, err := ExpandOrchestratedVirtualMachineScaleSetNetworkInterface(v.([]interface{}))
		if err != nil {
			return fmt.Errorf("expanding `network_interface`: %+v", err)
		}

		networkProfile.NetworkInterfaceConfigurations = networkInterfaces
		virtualMachineProfile.NetworkProfile = networkProfile
	}

	if v, ok := d.GetOk("extension"); ok {
		extensionProfile, err := expandOrchestratedVirtualMachineScaleSetExtensions(v.(*pluginsdk.Set).List())
		if err != nil {
			return err
		}
		virtualMachineProfile.ExtensionProfile = extensionProfile
	}

	if v, ok := d.GetOk("extensions_time_budget"); ok {
		if virtualMachineProfile.ExtensionProfile == nil {
			virtualMachineProfile.ExtensionProfile = &compute.VirtualMachineScaleSetExtensionProfile{}
		}
		virtualMachineProfile.ExtensionProfile.ExtensionsTimeBudget = utils.String(v.(string))
	}

	if v, ok := d.Get("max_bid_price").(float64); ok && v > 0 {
		if virtualMachineProfile.Priority != compute.VirtualMachinePriorityTypesSpot {
			return fmt.Errorf("`max_bid_price` can only be configured when `priority` is set to `Spot`")
		}

		virtualMachineProfile.BillingProfile = &compute.BillingProfile{
			MaxPrice: utils.Float(v),
		}
	}

	if v, ok := d.GetOk("encryption_at_host_enabled"); ok {
		virtualMachineProfile.SecurityProfile = &compute.SecurityProfile{
			EncryptionAtHost: utils.Bool(v.(bool)),
		}
	}

	if v, ok := d.GetOk("eviction_policy"); ok {
		if virtualMachineProfile.Priority != compute.VirtualMachinePriorityTypesSpot {
			return fmt.Errorf("an `eviction_policy` can only be specified when `priority` is set to `Spot`")
		}
		virtualMachineProfile.EvictionPolicy = compute.VirtualMachineEvictionPolicyTypes(v.(string))
	} else if virtualMachineProfile.Priority == compute.VirtualMachinePriorityTypesSpot {
		return fmt.Errorf("an `eviction_policy` must be specified when `priority` is set to `Spot`")
	}

	if v, ok := d.GetOk("license_type"); ok {
		virtualMachineProfile.LicenseType = utils.String(v.(string))
	}

	if v, ok := d.GetOk("terminate_notification"); ok {
		virtualMachineProfile.ScheduledEventsProfile = ExpandVirtualMachineScaleSetScheduledEventsProfile(v.([]interface{}))
	}

	// Only inclued the virtual machine profile if this is not a legacy configuration
	if !isLegacy {
		if v, ok := d.GetOk("plan"); ok {
			props.Plan = expandPlan(v.([]interface{}))
		}

		if v, ok := d.GetOk("identity"); ok {
			identity, err := ExpandVirtualMachineScaleSetIdentity(v.([]interface{}))
			if err != nil {
				return fmt.Errorf("expanding `identity`: %+v", err)
			}
			props.Identity = identity
		}

		if v, ok := d.GetOk("automatic_instance_repair"); ok {
			props.VirtualMachineScaleSetProperties.AutomaticRepairsPolicy = ExpandOrchestratedVirtualMachineScaleSetAutomaticRepairsPolicy(v.([]interface{}))
		}

		if v, ok := d.GetOk("zone_balance"); ok && v.(bool) {
			if len(*zones) == 0 {
				return fmt.Errorf("`zone_balance` can only be set to `true` when zones are specified")
			}

			props.VirtualMachineScaleSetProperties.ZoneBalance = utils.Bool(v.(bool))
		}

		props.VirtualMachineScaleSetProperties.VirtualMachineProfile = &virtualMachineProfile
	}

	log.Printf("[DEBUG] Creating Orchestrated Virtual Machine Scale Set %q (Resource Group %q)..", name, resourceGroup)
	future, err := client.CreateOrUpdate(ctx, resourceGroup, name, props)
	if err != nil {
		return fmt.Errorf("creating Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	log.Printf("[DEBUG] Waiting for Orchestrated Virtual Machine Scale Set %q (Resource Group %q) to be created..", name, resourceGroup)
	if err := future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("waiting for creation of Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", name, resourceGroup, err)
	}
	log.Printf("[DEBUG] Virtual Machine Scale Set %q (Resource Group %q) was created", name, resourceGroup)

	log.Printf("[DEBUG] Retrieving Virtual Machine Scale Set %q (Resource Group %q)..", name, resourceGroup)
	// Upgrading to the 2021-07-01 exposed a new expand parameter in the GET method
	resp, err := client.Get(ctx, resourceGroup, name, compute.ExpandTypesForGetVMScaleSetsUserData)
	if err != nil {
		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if resp.ID == nil || *resp.ID == "" {
		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): ID was nil", name, resourceGroup)
	}
	d.SetId(*resp.ID)

	return resourceOrchestratedVirtualMachineScaleSetRead(d, meta)
}

func resourceOrchestratedVirtualMachineScaleSetUpdate(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Compute.VMScaleSetClient
	ctx, cancel := timeouts.ForUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := parse.VirtualMachineScaleSetID(d.Id())
	if err != nil {
		return err
	}

	isLegacy := true
	updateInstances := false

	// retrieve
	// Upgrading to the 2021-07-01 exposed a new expand parameter in the GET method
	existing, err := client.Get(ctx, id.ResourceGroup, id.Name, compute.ExpandTypesForGetVMScaleSetsUserData)
	if err != nil {
		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", id.Name, id.ResourceGroup, err)
	}
	if existing.Sku != nil {
		isLegacy = false
	}
	if existing.VirtualMachineScaleSetProperties == nil {
		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): `properties` was nil", id.Name, id.ResourceGroup)
	}

	if !isLegacy {
		if existing.VirtualMachineScaleSetProperties.VirtualMachineProfile == nil {
			return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): `properties.virtualMachineProfile` was nil", id.Name, id.ResourceGroup)
		}
		if existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile == nil {
			return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): `properties.virtualMachineProfile,storageProfile` was nil", id.Name, id.ResourceGroup)
		}
	}

	updateProps := compute.VirtualMachineScaleSetUpdateProperties{}
	update := compute.VirtualMachineScaleSetUpdate{}
	osType := compute.OperatingSystemTypesWindows

	if !isLegacy {
		updateProps = compute.VirtualMachineScaleSetUpdateProperties{
			VirtualMachineProfile: &compute.VirtualMachineScaleSetUpdateVMProfile{
				// if an image reference has been configured previously (it has to be), we would better to include that in this
				// update request to avoid some circumstances that the API will complain ImageReference is null
				// issue tracking: https://github.com/Azure/azure-rest-api-specs/issues/10322
				StorageProfile: &compute.VirtualMachineScaleSetUpdateStorageProfile{
					ImageReference: existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.ImageReference,
				},
			},
			// Currently not suppored in orchestrated VMSS
			// if an upgrade policy's been configured previously (which it will have) it must be threaded through
			// this doesn't matter for Manual - but breaks when updating anything on a Automatic and Rolling Mode Scale Set
			// UpgradePolicy: existing.VirtualMachineScaleSetProperties.UpgradePolicy,
		}

		priority := compute.VirtualMachinePriorityTypes(d.Get("priority").(string))
		if d.HasChange("max_bid_price") {
			if priority != compute.VirtualMachinePriorityTypesSpot {
				return fmt.Errorf("`max_bid_price` can only be configured when `priority` is set to `Spot`")
			}

			updateProps.VirtualMachineProfile.BillingProfile = &compute.BillingProfile{
				MaxPrice: utils.Float(d.Get("max_bid_price").(float64)),
			}
		}

		osProfileRaw := d.Get("os_profile").([]interface{})
		vmssOsProfile := compute.VirtualMachineScaleSetUpdateOSProfile{}
		windowsConfig := compute.WindowsConfiguration{}
		linuxConfig := compute.LinuxConfiguration{}

		if len(osProfileRaw) > 0 {
			osProfile := osProfileRaw[0].(map[string]interface{})
			winConfigRaw := osProfile["windows_configuration"].([]interface{})
			linConfigRaw := osProfile["linux_configuration"].([]interface{})

			if d.HasChange("os_profile.0.custom_data") {
				updateInstances = true

				// customData can only be sent if it's a base64 encoded string,
				// so it's not possible to remove this without tainting the resource
				vmssOsProfile.CustomData = utils.String(osProfile["custom_data"].(string))
			}

			if len(winConfigRaw) > 0 {
				winConfig := winConfigRaw[0].(map[string]interface{})

				if d.HasChange("os_profile.0.windows_configuration.0.enable_automatic_updates") ||
					d.HasChange("os_profile.0.windows_configuration.0.provision_vm_agent") ||
					d.HasChange("os_profile.0.windows_configuration.0.timezone") ||
					d.HasChange("os_profile.0.windows_configuration.0.secret") ||
					d.HasChange("os_profile.0.windows_configuration.0.winrm_listener") {
					updateInstances = true
				}

				if d.HasChange("os_profile.0.windows_configuration.0.enable_automatic_updates") {
					windowsConfig.EnableAutomaticUpdates = utils.Bool(winConfig["enable_automatic_updates"].(bool))
				}

				if d.HasChange("os_profile.0.windows_configuration.0.provision_vm_agent") {
					windowsConfig.ProvisionVMAgent = utils.Bool(winConfig["provision_vm_agent"].(bool))
				}

				if d.HasChange("os_profile.0.windows_configuration.0.timezone") {
					windowsConfig.TimeZone = utils.String(winConfig["timezone"].(string))
				}

				if d.HasChange("os_profile.0.windows_configuration.0.secret") {
					vmssOsProfile.Secrets = expandWindowsSecrets(winConfig["secret"].([]interface{}))
				}

				if d.HasChange("os_profile.0.windows_configuration.0.winrm_listener") {
					winRmListenersRaw := winConfig["winrm_listener"].(*pluginsdk.Set).List()
					vmssOsProfile.WindowsConfiguration.WinRM = expandWinRMListener(winRmListenersRaw)
				}

				vmssOsProfile.WindowsConfiguration = &windowsConfig
			}

			if len(linConfigRaw) > 0 {
				osType = compute.OperatingSystemTypesLinux
				linConfig := linConfigRaw[0].(map[string]interface{})

				if d.HasChange("os_profile.0.linux_configuration.0.provision_vm_agent") ||
					d.HasChange("os_profile.0.linux_configuration.0.disable_password_authentication") ||
					d.HasChange("os_profile.0.linux_configuration.0.admin_ssh_key") {
					updateInstances = true
				}

				if d.HasChange("os_profile.0.linux_configuration.0.provision_vm_agent") {
					linuxConfig.ProvisionVMAgent = utils.Bool(linConfig["provision_vm_agent"].(bool))
				}

				if d.HasChange("os_profile.0.linux_configuration.0.disable_password_authentication") {
					linuxConfig.DisablePasswordAuthentication = utils.Bool(linConfig["disable_password_authentication"].(bool))
				}

				if d.HasChange("os_profile.0.linux_configuration.0.admin_ssh_key") {
					sshPublicKeys := ExpandSSHKeys(linConfig["admin_ssh_key"].(*pluginsdk.Set).List())
					if linuxConfig.SSH == nil {
						linuxConfig.SSH = &compute.SSHConfiguration{}
					}
					linuxConfig.SSH.PublicKeys = &sshPublicKeys
				}

				vmssOsProfile.LinuxConfiguration = &linuxConfig
			}

			updateProps.VirtualMachineProfile.OsProfile = &vmssOsProfile
		}

		if d.HasChange("data_disk") || d.HasChange("os_disk") || d.HasChange("source_image_id") || d.HasChange("source_image_reference") {
			updateInstances = true

			if updateProps.VirtualMachineProfile.StorageProfile == nil {
				updateProps.VirtualMachineProfile.StorageProfile = &compute.VirtualMachineScaleSetUpdateStorageProfile{}
			}

			if d.HasChange("data_disk") {
				ultraSSDEnabled := false // Currently not supported in orchestrated vmss
				dataDisks, err := ExpandOrchestratedVirtualMachineScaleSetDataDisk(d.Get("data_disk").([]interface{}), ultraSSDEnabled)
				if err != nil {
					return fmt.Errorf("expanding `data_disk`: %+v", err)
				}
				updateProps.VirtualMachineProfile.StorageProfile.DataDisks = dataDisks
			}

			if d.HasChange("os_disk") {
				osDiskRaw := d.Get("os_disk").([]interface{})
				updateProps.VirtualMachineProfile.StorageProfile.OsDisk = ExpandOrchestratedVirtualMachineScaleSetOSDiskUpdate(osDiskRaw)
			}

			if d.HasChange("source_image_id") || d.HasChange("source_image_reference") {
				sourceImageReferenceRaw := d.Get("source_image_reference").([]interface{})
				sourceImageId := d.Get("source_image_id").(string)
				sourceImageReference, err := expandSourceImageReference(sourceImageReferenceRaw, sourceImageId)
				if err != nil {
					return err
				}

				// Must include all storage profile properties when updating disk image.  See: https://github.com/hashicorp/terraform-provider-azurerm/issues/8273
				updateProps.VirtualMachineProfile.StorageProfile.DataDisks = existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.DataDisks
				updateProps.VirtualMachineProfile.StorageProfile.ImageReference = sourceImageReference
				updateProps.VirtualMachineProfile.StorageProfile.OsDisk = &compute.VirtualMachineScaleSetUpdateOSDisk{
					Caching:                 existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.OsDisk.Caching,
					WriteAcceleratorEnabled: existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.OsDisk.WriteAcceleratorEnabled,
					DiskSizeGB:              existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.OsDisk.DiskSizeGB,
					Image:                   existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.OsDisk.Image,
					VhdContainers:           existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.OsDisk.VhdContainers,
					ManagedDisk:             existing.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile.OsDisk.ManagedDisk,
				}
			}
		}

		if d.HasChange("network_interface") {
			networkInterfacesRaw := d.Get("network_interface").([]interface{})
			networkInterfaces, err := ExpandOrchestratedVirtualMachineScaleSetNetworkInterfaceUpdate(networkInterfacesRaw)
			if err != nil {
				return fmt.Errorf("expanding `network_interface`: %+v", err)
			}

			updateProps.VirtualMachineProfile.NetworkProfile = &compute.VirtualMachineScaleSetUpdateNetworkProfile{
				NetworkInterfaceConfigurations: networkInterfaces,
				// 2020-11-01 is the only valid value for this value and is only valid for VMSS in Orchestration Mode flex
				NetworkAPIVersion: compute.NetworkAPIVersionTwoZeroTwoZeroHyphenMinusOneOneHyphenMinusZeroOne,
			}
		}

		if d.HasChange("boot_diagnostics") {
			updateInstances = true

			bootDiagnosticsRaw := d.Get("boot_diagnostics").([]interface{})
			updateProps.VirtualMachineProfile.DiagnosticsProfile = expandBootDiagnostics(bootDiagnosticsRaw)
		}

		if d.HasChange("terminate_notification") {
			notificationRaw := d.Get("terminate_notification").([]interface{})
			updateProps.VirtualMachineProfile.ScheduledEventsProfile = ExpandOrchestratedVirtualMachineScaleSetScheduledEventsProfile(notificationRaw)
		}

		if d.HasChange("encryption_at_host_enabled") {
			updateProps.VirtualMachineProfile.SecurityProfile = &compute.SecurityProfile{
				EncryptionAtHost: utils.Bool(d.Get("encryption_at_host_enabled").(bool)),
			}
		}

		if d.HasChange("license_type") {
			license := d.Get("license_type").(string)
			if license == "" {
				// Only for create no specification is possible in the API. API does not allow empty string in update.
				// So removing attribute license_type from Terraform configuration if it was set to value other than 'None' would lead to an endless loop in apply.
				// To allow updating in this case set value explicitly to 'None'.
				license = "None"
			}
			updateProps.VirtualMachineProfile.LicenseType = &license
		}

		if d.HasChange("automatic_instance_repair") {
			automaticRepairsPolicyRaw := d.Get("automatic_instance_repair").([]interface{})
			automaticRepairsPolicy := ExpandOrchestratedVirtualMachineScaleSetAutomaticRepairsPolicy(automaticRepairsPolicyRaw)
			updateProps.AutomaticRepairsPolicy = automaticRepairsPolicy
		}

		if d.HasChange("identity") {
			identityRaw := d.Get("identity").([]interface{})
			identity, err := ExpandOrchestratedVirtualMachineScaleSetIdentity(identityRaw)
			if err != nil {
				return fmt.Errorf("expanding `identity`: %+v", err)
			}

			update.Identity = identity
		}

		if d.HasChange("plan") {
			planRaw := d.Get("plan").([]interface{})
			update.Plan = expandPlan(planRaw)
		}

		if d.HasChange("sku_name") {
			// in-case ignore_changes is being used, since both fields are required
			// look up the current values and override them as needed
			sku := existing.Sku

			if d.HasChange("sku_name") {
				updateInstances = true
				sku, err = azure.ExpandOrchestratedVirtualMachineScaleSetSku(d.Get("sku").(string))
				if err != nil {
					return err
				}
			}

			update.Sku = sku
		}

		if d.HasChanges("extension", "extensions_time_budget") {
			updateInstances = true

			extensionProfile, err := expandOrchestratedVirtualMachineScaleSetExtensions(d.Get("extension").(*pluginsdk.Set).List())
			if err != nil {
				return err
			}
			updateProps.VirtualMachineProfile.ExtensionProfile = extensionProfile
			updateProps.VirtualMachineProfile.ExtensionProfile.ExtensionsTimeBudget = utils.String(d.Get("extensions_time_budget").(string))
		}
	}

	// Only two fields that can change in legacy mode
	if d.HasChange("proximity_placement_group_id") {
		if v, ok := d.GetOk("proximity_placement_group_id"); ok {
			updateInstances = true
			updateProps.ProximityPlacementGroup = &compute.SubResource{
				ID: utils.String(v.(string)),
			}
		}
	}

	if d.HasChange("tags") {
		update.Tags = tags.Expand(d.Get("tags").(map[string]interface{}))
	}

	update.VirtualMachineScaleSetUpdateProperties = &updateProps

	if updateInstances {
		log.Printf("[DEBUG] Orchestrated Virtual Machine Scale Set %q in Resource Group %q - updateInstances is true", id.Name, id.ResourceGroup)
	}

	// AutomaticOSUpgradeIsEnabled currently is not supported in orchestrated VMSS flex
	metaData := virtualMachineScaleSetUpdateMetaData{
		AutomaticOSUpgradeIsEnabled: false,
		// CanRollInstancesWhenRequired: meta.(*clients.Client).Features.VirtualMachineScaleSet.RollInstancesWhenRequired,
		// UpdateInstances:              updateInstances,
		CanRollInstancesWhenRequired: false,
		UpdateInstances:              false,
		Client:                       meta.(*clients.Client).Compute,
		Existing:                     existing,
		ID:                           id,
		OSType:                       osType,
	}

	if err := metaData.performUpdate(ctx, update); err != nil {
		return err
	}

	return resourceOrchestratedVirtualMachineScaleSetRead(d, meta)
}

func resourceOrchestratedVirtualMachineScaleSetRead(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Compute.VMScaleSetClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := parse.VirtualMachineScaleSetID(d.Id())
	if err != nil {
		return err
	}

	// Upgrading to the 2021-07-01 exposed a new expand parameter in the GET method
	resp, err := client.Get(ctx, id.ResourceGroup, id.Name, compute.ExpandTypesForGetVMScaleSetsUserData)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[DEBUG] Orchestrated Virtual Machine Scale Set %q was not found in Resource Group %q - removing from state!", id.Name, id.ResourceGroup)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", id.Name, id.ResourceGroup, err)
	}

	d.Set("name", id.Name)
	d.Set("resource_group_name", id.ResourceGroup)
	d.Set("location", location.NormalizeNilable(resp.Location))

	var skuName *string
	if resp.Sku != nil {
		skuName, err = azure.FlattenOrchestratedVirtualMachineScaleSetSku(resp.Sku)
		if err != nil || skuName == nil {
			return fmt.Errorf("setting `sku_name`: %+v", err)
		}

		d.Set("sku_name", skuName)
	}

	identity, err := FlattenOrchestratedVirtualMachineScaleSetIdentity(resp.Identity)
	if err != nil {
		return err
	}
	if err := d.Set("identity", identity); err != nil {
		return fmt.Errorf("setting `identity`: %+v", err)
	}

	if err := d.Set("plan", flattenPlan(resp.Plan)); err != nil {
		return fmt.Errorf("setting `plan`: %+v", err)
	}

	if resp.VirtualMachineScaleSetProperties == nil {
		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): `properties` was nil", id.Name, id.ResourceGroup)
	}
	props := *resp.VirtualMachineScaleSetProperties

	if err := d.Set("automatic_instance_repair", FlattenOrchestratedVirtualMachineScaleSetAutomaticRepairsPolicy(props.AutomaticRepairsPolicy)); err != nil {
		return fmt.Errorf("setting `automatic_instance_repair`: %+v", err)
	}

	d.Set("platform_fault_domain_count", props.PlatformFaultDomainCount)
	proximityPlacementGroupId := ""
	if props.ProximityPlacementGroup != nil && props.ProximityPlacementGroup.ID != nil {
		proximityPlacementGroupId = *props.ProximityPlacementGroup.ID
	}
	d.Set("proximity_placement_group_id", proximityPlacementGroupId)
	d.Set("unique_id", props.UniqueID)
	d.Set("zone_balance", props.ZoneBalance)

	if profile := props.VirtualMachineProfile; profile != nil {
		if err := d.Set("boot_diagnostics", flattenBootDiagnostics(profile.DiagnosticsProfile)); err != nil {
			return fmt.Errorf("setting `boot_diagnostics`: %+v", err)
		}

		// defaulted since BillingProfile isn't returned if it's unset
		maxBidPrice := float64(-1.0)
		if profile.BillingProfile != nil && profile.BillingProfile.MaxPrice != nil {
			maxBidPrice = *profile.BillingProfile.MaxPrice
		}
		d.Set("max_bid_price", maxBidPrice)

		d.Set("eviction_policy", string(profile.EvictionPolicy))
		d.Set("license_type", profile.LicenseType)

		// the service just return empty when this is not assigned when provisioned
		// See discussion on https://github.com/Azure/azure-rest-api-specs/issues/10971
		priority := compute.VirtualMachinePriorityTypesRegular
		if profile.Priority != "" {
			priority = profile.Priority
		}
		d.Set("priority", priority)

		if storageProfile := profile.StorageProfile; storageProfile != nil {
			if err := d.Set("os_disk", FlattenOrchestratedVirtualMachineScaleSetOSDisk(storageProfile.OsDisk)); err != nil {
				return fmt.Errorf("setting `os_disk`: %+v", err)
			}

			if err := d.Set("data_disk", FlattenOrchestratedVirtualMachineScaleSetDataDisk(storageProfile.DataDisks)); err != nil {
				return fmt.Errorf("setting `data_disk`: %+v", err)
			}

			if err := d.Set("source_image_reference", flattenSourceImageReference(storageProfile.ImageReference)); err != nil {
				return fmt.Errorf("setting `source_image_reference`: %+v", err)
			}

			var storageImageId string
			if storageProfile.ImageReference != nil && storageProfile.ImageReference.ID != nil {
				storageImageId = *storageProfile.ImageReference.ID
			}
			d.Set("source_image_id", storageImageId)
		}

		if osProfile := profile.OsProfile; osProfile != nil {
			if err := d.Set("os_profile", FlattenOrchestratedVirtualMachineScaleSetOSProfile(osProfile, d)); err != nil {
				return fmt.Errorf("setting `os_profile`: %+v", err)
			}
		}

		if nwProfile := profile.NetworkProfile; nwProfile != nil {
			flattenedNics := FlattenOrchestratedVirtualMachineScaleSetNetworkInterface(nwProfile.NetworkInterfaceConfigurations)
			if err := d.Set("network_interface", flattenedNics); err != nil {
				return fmt.Errorf("setting `network_interface`: %+v", err)
			}
		}

		if scheduleProfile := profile.ScheduledEventsProfile; scheduleProfile != nil {
			if err := d.Set("terminate_notification", FlattenOrchestratedVirtualMachineScaleSetScheduledEventsProfile(scheduleProfile)); err != nil {
				return fmt.Errorf("setting `terminate_notification`: %+v", err)
			}
		}

		extensionProfile, err := flattenOrchestratedVirtualMachineScaleSetExtensions(profile.ExtensionProfile, d)
		if err != nil {
			return fmt.Errorf("failed flattening `extension`: %+v", err)
		}
		d.Set("extension", extensionProfile)

		extensionsTimeBudget := "PT1H30M"
		if profile.ExtensionProfile != nil && profile.ExtensionProfile.ExtensionsTimeBudget != nil {
			extensionsTimeBudget = *profile.ExtensionProfile.ExtensionsTimeBudget
		}
		d.Set("extensions_time_budget", extensionsTimeBudget)

		encryptionAtHostEnabled := false
		if profile.SecurityProfile != nil && profile.SecurityProfile.EncryptionAtHost != nil {
			encryptionAtHostEnabled = *profile.SecurityProfile.EncryptionAtHost
		}
		d.Set("encryption_at_host_enabled", encryptionAtHostEnabled)
	}

	if err := d.Set("zones", resp.Zones); err != nil {
		return fmt.Errorf("setting `zones`: %+v", err)
	}

	return tags.FlattenAndSet(d, resp.Tags)
}

func resourceOrchestratedVirtualMachineScaleSetDelete(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Compute.VMScaleSetClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := parse.VirtualMachineScaleSetID(d.Id())
	if err != nil {
		return err
	}

	// Upgrading to the 2021-07-01 exposed a new expand parameter in the GET method
	resp, err := client.Get(ctx, id.ResourceGroup, id.Name, compute.ExpandTypesForGetVMScaleSetsUserData)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			return nil
		}

		return fmt.Errorf("retrieving Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", id.Name, id.ResourceGroup, err)
	}

	// Sometimes VMSS's aren't fully deleted when the `Delete` call returns - as such we'll try to scale the cluster
	// to 0 nodes first, then delete the cluster - which should ensure there's no Network Interfaces kicking around
	// and work around this Azure API bug:
	// Original Error: Code="InUseSubnetCannotBeDeleted" Message="Subnet internal is in use by
	// /{nicResourceID}/|providers|Microsoft.Compute|virtualMachineScaleSets|acctestvmss-190923101253410278|virtualMachines|0|networkInterfaces|example/ipConfigurations/internal and cannot be deleted.
	// In order to delete the subnet, delete all the resources within the subnet. See aka.ms/deletesubnet.
	if resp.Sku != nil {
		resp.Sku.Capacity = utils.Int64(int64(0))

		log.Printf("[DEBUG] Scaling instances to 0 prior to deletion - this helps avoids networking issues within Azure")
		update := compute.VirtualMachineScaleSetUpdate{
			Sku: resp.Sku,
		}
		future, err := client.Update(ctx, id.ResourceGroup, id.Name, update)
		if err != nil {
			return fmt.Errorf("updating number of instances in Orchestrated Virtual Machine Scale Set %q (Resource Group %q) to scale to 0: %+v", id.Name, id.ResourceGroup, err)
		}

		log.Printf("[DEBUG] Waiting for scaling of instances to 0 prior to deletion - this helps avoids networking issues within Azure")
		err = future.WaitForCompletionRef(ctx, client.Client)
		if err != nil {
			return fmt.Errorf("waiting for number of instances in Orchestrated Virtual Machine Scale Set %q (Resource Group %q) to scale to 0: %+v", id.Name, id.ResourceGroup, err)
		}
		log.Printf("[DEBUG] Scaled instances to 0 prior to deletion - this helps avoids networking issues within Azure")
	} else {
		log.Printf("[DEBUG] Unable to scale instances to `0` since the `sku` block is nil - trying to delete anyway")
	}

	log.Printf("[DEBUG] Deleting Orchestrated Virtual Machine Scale Set %q (Resource Group %q)..", id.Name, id.ResourceGroup)
	// @ArcturusZhang (mimicking from windows_virtual_machine_pluginsdk.go): sending `nil` here omits this value from being sent
	// which matches the previous behaviour - we're only splitting this out so it's clear why
	// TODO: support force deletion once it's out of Preview, if applicable
	var forceDeletion *bool = nil
	future, err := client.Delete(ctx, id.ResourceGroup, id.Name, forceDeletion)
	if err != nil {
		return fmt.Errorf("deleting Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", id.Name, id.ResourceGroup, err)
	}

	log.Printf("[DEBUG] Waiting for deletion of Orchestrated Virtual Machine Scale Set %q (Resource Group %q)..", id.Name, id.ResourceGroup)
	if err := future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("waiting for deletion of Orchestrated Virtual Machine Scale Set %q (Resource Group %q): %+v", id.Name, id.ResourceGroup, err)
	}
	log.Printf("[DEBUG] Deleted Orchestrated Virtual Machine Scale Set %q (Resource Group %q).", id.Name, id.ResourceGroup)

	return nil
}
