/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package admitters

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"

	"k8s.io/api/admission/v1beta1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/kubevirt/pkg/hooks"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

const (
	arrayLenMax = 256
	maxStrLen   = 256

	// cloudInitNetworkMaxLen and CloudInitUserMaxLen are being limited
	// to 2K to allow scaling of config as edits will cause entire object
	// to be distributed to large no of nodes. For larger than 2K, user should
	// use NetworkDataSecretRef and UserDataSecretRef
	cloudInitUserMaxLen    = 2048
	cloudInitNetworkMaxLen = 2048

	// Copied from kubernetes/pkg/apis/core/validation/validation.go
	maxDNSNameservers     = 3
	maxDNSSearchPaths     = 6
	maxDNSSearchListChars = 256
)

var validInterfaceModels = []string{"e1000", "e1000e", "ne2k_pci", "pcnet", "rtl8139", "virtio"}
var validIOThreadsPolicies = []v1.IOThreadsPolicy{v1.IOThreadsPolicyShared, v1.IOThreadsPolicyAuto}
var validCPUFeaturePolicies = []string{"", "force", "require", "optional", "disable", "forbid"}

type VMICreateAdmitter struct {
	ClusterConfig *virtconfig.ClusterConfig
}

func (admitter *VMICreateAdmitter) Admit(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	if resp := webhooks.ValidateSchema(v1.VirtualMachineInstanceGroupVersionKind, ar.Request.Object.Raw); resp != nil {
		return resp
	}

	vmi, _, err := getAdmissionReviewVMI(ar)
	if err != nil {
		return webhooks.ToAdmissionResponseError(err)
	}

	causes := ValidateVirtualMachineInstanceSpec(k8sfield.NewPath("spec"), &vmi.Spec, admitter.ClusterConfig)
	causes = append(causes, ValidateVirtualMachineInstanceMandatoryFields(k8sfield.NewPath("spec"), &vmi.Spec)...)
	causes = append(causes, ValidateVirtualMachineInstanceMetadata(k8sfield.NewPath("metadata"), &vmi.ObjectMeta, admitter.ClusterConfig)...)
	// In a future, yet undecided, release either libvirt or QEMU are going to check the hyperv dependencies, so we can get rid of this code.
	causes = append(causes, webhooks.ValidateVirtualMachineInstanceHypervFeatureDependencies(k8sfield.NewPath("spec"), &vmi.Spec)...)

	if len(causes) > 0 {
		return webhooks.ToAdmissionResponse(causes)
	}

	reviewResponse := v1beta1.AdmissionResponse{}
	reviewResponse.Allowed = true
	return &reviewResponse
}

func ValidateVirtualMachineInstanceSpec(field *k8sfield.Path, spec *v1.VirtualMachineInstanceSpec, config *virtconfig.ClusterConfig) []metav1.StatusCause {
	var causes []metav1.StatusCause
	volumeNameMap := make(map[string]*v1.Volume)
	networkNameMap := make(map[string]*v1.Network)

	if len(spec.Domain.Devices.Disks) > arrayLenMax {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s list exceeds the %d element limit in length", field.Child("domain", "devices", "disks").String(), arrayLenMax),
			Field:   field.Child("domain", "devices", "disks").String(),
		})
		// We won't process anything over the limit
		return causes
	} else if len(spec.Volumes) > arrayLenMax {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s list exceeds the %d element limit in length", field.Child("volumes").String(), arrayLenMax),
			Field:   field.Child("volumes").String(),
		})
		// We won't process anything over the limit
		return causes
	}
	// Validate hostname according to DNS label rules
	if spec.Hostname != "" {
		errors := validation.IsDNS1123Label(spec.Hostname)
		if len(errors) != 0 {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s does not conform to the kubernetes DNS_LABEL rules : %s",
					field.Child("hostname").String(), strings.Join(errors, ", ")),
				Field: field.Child("hostname").String(),
			})
		}
	}

	// Validate subdomain according to DNS subdomain rules
	if spec.Subdomain != "" {
		errors := validation.IsDNS1123Subdomain(spec.Subdomain)
		if len(errors) != 0 {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s does not conform to the kubernetes DNS_SUBDOMAIN rules : %s",
					field.Child("subdomain").String(), strings.Join(errors, ", ")),
				Field: field.Child("subdomain").String(),
			})
		}
	}

	// Validate memory size if values are not negative or too small
	if spec.Domain.Resources.Requests.Memory().Value() < 0 {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s': must be greater than or equal to 0.", field.Child("domain", "resources", "requests", "memory").String(),
				spec.Domain.Resources.Requests.Memory()),
			Field: field.Child("domain", "resources", "requests", "memory").String(),
		})
	} else if spec.Domain.Resources.Requests.Memory().Value() > 0 && spec.Domain.Resources.Requests.Memory().Cmp(resource.MustParse("1M")) < 0 {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s': must be greater than or equal to 1M.", field.Child("domain", "resources", "requests", "memory").String(),
				spec.Domain.Resources.Requests.Memory()),
			Field: field.Child("domain", "resources", "requests", "memory").String(),
		})
	}

	if spec.Domain.Resources.Limits.Memory().Value() < 0 {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s': must be greater than or equal to 0.", field.Child("domain", "resources", "limits", "memory").String(),
				spec.Domain.Resources.Limits.Memory()),
			Field: field.Child("domain", "resources", "limits", "memory").String(),
		})
	}

	if spec.Domain.Resources.Limits.Memory().Value() > 0 &&
		spec.Domain.Resources.Requests.Memory().Value() > spec.Domain.Resources.Limits.Memory().Value() {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s' is greater than %s '%s'", field.Child("domain", "resources", "requests", "memory").String(),
				spec.Domain.Resources.Requests.Memory(),
				field.Child("domain", "resources", "limits", "memory").String(),
				spec.Domain.Resources.Limits.Memory()),
			Field: field.Child("domain", "resources", "requests", "memory").String(),
		})
	}

	if spec.Domain.Memory != nil && spec.Domain.Memory.Hugepages != nil && spec.Domain.Memory.Guest != nil {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("'%s' and '%s' must not be set at the same time",
				field.Child("domain", "memory", "guest").String(),
				field.Child("domain", "memory", "hugepages", "size").String()),
			Field: field.Child("domain", "resources", "requests", "memory").String(),
		})
	}

	// Validate hugepages
	if spec.Domain.Memory != nil && spec.Domain.Memory.Hugepages != nil {
		hugepagesSize, err := resource.ParseQuantity(spec.Domain.Memory.Hugepages.PageSize)
		if err != nil {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s '%s': %s",
					field.Child("domain", "hugepages", "size").String(),
					spec.Domain.Memory.Hugepages.PageSize,
					resource.ErrFormatWrong,
				),
				Field: field.Child("domain", "hugepages", "size").String(),
			})
		} else {
			vmMemory := spec.Domain.Resources.Requests.Memory().Value()
			if vmMemory < hugepagesSize.Value() {
				causes = append(causes, metav1.StatusCause{
					Type: metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s '%s' must be equal to or larger than page size %s '%s'",
						field.Child("domain", "resources", "requests", "memory").String(),
						spec.Domain.Resources.Requests.Memory(),
						field.Child("domain", "hugepages", "size").String(),
						spec.Domain.Memory.Hugepages.PageSize,
					),
					Field: field.Child("domain", "resources", "requests", "memory").String(),
				})
			} else if vmMemory%hugepagesSize.Value() != 0 {
				causes = append(causes, metav1.StatusCause{
					Type: metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s '%s' is not a multiple of the page size %s '%s'",
						field.Child("domain", "resources", "requests", "memory").String(),
						spec.Domain.Resources.Requests.Memory(),
						field.Child("domain", "hugepages", "size").String(),
						spec.Domain.Memory.Hugepages.PageSize,
					),
					Field: field.Child("domain", "resources", "requests", "memory").String(),
				})
			}
		}
	}
	// Validate hugepages
	if spec.Domain.Memory != nil && spec.Domain.Memory.Guest != nil {
		requests := spec.Domain.Resources.Requests.Memory().Value()
		limits := spec.Domain.Resources.Limits.Memory().Value()
		guest := spec.Domain.Memory.Guest.Value()
		if requests > guest {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s '%s' must be equal to or larger than the requested memory %s '%s'",
					field.Child("domain", "memory", "guest").String(),
					spec.Domain.Memory.Guest,
					field.Child("domain", "resources", "requests", "memory").String(),
					spec.Domain.Resources.Requests.Memory(),
				),
				Field: field.Child("domain", "memory", "guest").String(),
			})
		}
		if limits < guest && limits != 0 {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s '%s' must be equal to or less than the memory limit %s '%s'",
					field.Child("domain", "memory", "guest").String(),
					spec.Domain.Memory.Guest,
					field.Child("domain", "resources", "limits", "memory").String(),
					spec.Domain.Resources.Limits.Memory(),
				),
				Field: field.Child("domain", "memory", "guest").String(),
			})
		}
	}

	// Validate emulated machine
	if len(spec.Domain.Machine.Type) > 0 {
		machine := spec.Domain.Machine.Type
		supportedMachines := config.GetEmulatedMachines()
		var match = false
		for _, val := range supportedMachines {
			if regexp.MustCompile(val).MatchString(machine) {
				match = true
			}
		}
		if !match {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s is not supported: %s (allowed values: %v)",
					field.Child("domain", "machine", "type").String(),
					machine,
					supportedMachines,
				),
				Field: field.Child("domain", "machine", "type").String(),
			})
		}
	}

	if spec.Domain.Firmware != nil && len(spec.Domain.Firmware.Serial) > 0 {
		// Verify serial number is within valid length, if provided
		if len(spec.Domain.Firmware.Serial) > maxStrLen {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must be less than or equal to %d in length, if specified",
					field.Child("domain", "firmware", "serial").String(),
					maxStrLen,
				),
				Field: field.Child("domain", "firmware", "serial").String(),
			})
		}
		// Verify serial number is made up of valid characters for libvirt, if provided
		isValid := regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`).MatchString
		if !isValid(spec.Domain.Firmware.Serial) {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must be made up of the following characters [A-Za-z0-9_.+-], if specified", field.Child("domain", "firmware", "serial").String()),
				Field:   field.Child("domain", "firmware", "serial").String(),
			})
		}
	}

	// Validate cpu if values are not negative
	if spec.Domain.Resources.Requests.Cpu().MilliValue() < 0 {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s': must be greater than or equal to 0.", field.Child("domain", "resources", "requests", "cpu").String(),
				spec.Domain.Resources.Requests.Cpu()),
			Field: field.Child("domain", "resources", "requests", "cpu").String(),
		})
	}

	if spec.Domain.Resources.Limits.Cpu().MilliValue() < 0 {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s': must be greater than or equal to 0.", field.Child("domain", "resources", "limits", "cpu").String(),
				spec.Domain.Resources.Limits.Cpu()),
			Field: field.Child("domain", "resources", "limits", "cpu").String(),
		})
	}

	if spec.Domain.Resources.Limits.Cpu().MilliValue() > 0 &&
		spec.Domain.Resources.Requests.Cpu().MilliValue() > spec.Domain.Resources.Limits.Cpu().MilliValue() {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s '%s' is greater than %s '%s'", field.Child("domain", "resources", "requests", "cpu").String(),
				spec.Domain.Resources.Requests.Cpu(),
				field.Child("domain", "resources", "limits", "cpu").String(),
				spec.Domain.Resources.Limits.Cpu()),
			Field: field.Child("domain", "resources", "requests", "cpu").String(),
		})
	}

	// Validate CPU pinning
	if spec.Domain.CPU != nil && spec.Domain.CPU.DedicatedCPUPlacement {
		requestsMem := spec.Domain.Resources.Requests.Memory().Value()
		limitsMem := spec.Domain.Resources.Limits.Memory().Value()
		requestsCPU := spec.Domain.Resources.Requests.Cpu().Value()
		limitsCPU := spec.Domain.Resources.Limits.Cpu().Value()
		vCPUs := hardware.GetNumberOfVCPUs(spec.Domain.CPU)

		// memory should be provided
		if limitsMem == 0 && requestsMem == 0 {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s or %s should be provided",
					field.Child("domain", "resources", "requests", "memory").String(),
					field.Child("domain", "resources", "limits", "memory").String(),
				),
				Field: field.Child("domain", "resources", "limits", "memory").String(),
			})
		}

		// provided CPU requests must be an interger
		if requestsCPU > 0 && requestsCPU*1000 != spec.Domain.Resources.Requests.Cpu().MilliValue() {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: "provided resources CPU requests must be an interger",
				Field:   field.Child("domain", "resources", "requests", "cpu").String(),
			})
		}

		// provided CPU limits must be an interger
		if limitsCPU > 0 && limitsCPU*1000 != spec.Domain.Resources.Limits.Cpu().MilliValue() {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: "provided resources CPU limits must be an interger",
				Field:   field.Child("domain", "resources", "limits", "cpu").String(),
			})
		}

		// resources requests must be equal to limits
		if requestsMem > 0 && limitsMem > 0 && requestsMem != limitsMem {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must be equal to %s",
					field.Child("domain", "resources", "requests", "memory").String(),
					field.Child("domain", "resources", "limits", "memory").String(),
				),
				Field: field.Child("domain", "resources", "requests", "memory").String(),
			})
		}

		// cpu amount should be provided
		if requestsCPU == 0 && limitsCPU == 0 && vCPUs == 0 {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("either %s or %s or %s must be provided when DedicatedCPUPlacement is true ",
					field.Child("domain", "resources", "requests", "cpu").String(),
					field.Child("domain", "resources", "limits", "cpu").String(),
					field.Child("domain", "cpu", "cores").String(),
				),
				Field: field.Child("domain", "cpu", "dedicatedCpuPlacement").String(),
			})
		}

		// cpu amount must be provided
		if requestsCPU > 0 && limitsCPU > 0 && requestsCPU != limitsCPU {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s or %s must be equal when DedicatedCPUPlacement is true ",
					field.Child("domain", "resources", "requests", "cpu").String(),
					field.Child("domain", "resources", "limits", "cpu").String(),
				),
				Field: field.Child("domain", "cpu", "dedicatedCpuPlacement").String(),
			})
		}

		// cpu resource and cpu cores should not be provided together - unless both are equal
		if (requestsCPU > 0 || limitsCPU > 0) && vCPUs > 0 &&
			requestsCPU != vCPUs && limitsCPU != vCPUs {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s or %s must not be provided at the same time with %s when DedicatedCPUPlacement is true ",
					field.Child("domain", "resources", "requests", "cpu").String(),
					field.Child("domain", "resources", "limits", "cpu").String(),
					field.Child("domain", "cpu", "cores").String(),
				),
				Field: field.Child("domain", "cpu", "dedicatedCpuPlacement").String(),
			})
		}
	}

	// Validate CPU Feature Policies
	if spec.Domain.CPU != nil && spec.Domain.CPU.Features != nil {
		isValidPolicy := func(policy string) bool {
			for _, p := range validCPUFeaturePolicies {
				if p == policy {
					return true
				}
			}
			return false
		}
		for idx, feature := range spec.Domain.CPU.Features {
			if !isValidPolicy(feature.Policy) {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueNotSupported,
					Message: fmt.Sprintf("CPU feature %s uses policy %s that is not supported.", feature.Name, feature.Policy),
					Field:   field.Child("domain", "cpu", "features").Index(idx).Child("policy").String(),
				})
			}
		}
	}

	podNetworkInterfacePresent := false

	if len(spec.Domain.Devices.Interfaces) > arrayLenMax {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s list exceeds the %d element limit in length", field.Child("domain", "devices", "interfaces").String(), arrayLenMax),
			Field:   field.Child("domain", "devices", "interfaces").String(),
		})
		return causes
	} else if len(spec.Networks) > arrayLenMax {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s list exceeds the %d element limit in length", field.Child("networks").String(), arrayLenMax),
			Field:   field.Child("networks").String(),
		})
		return causes
	} else if num := getNumberOfPodInterfaces(spec); num >= 1 {
		podNetworkInterfacePresent = true
		if num > 1 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueDuplicate,
				Message: fmt.Sprintf("more than one interface is connected to a pod network in %s", field.Child("interfaces").String()),
				Field:   field.Child("interfaces").String(),
			})
			return causes
		}
	}

	for _, volume := range spec.Volumes {
		volumeNameMap[volume.Name] = &volume
	}

	// used to validate uniqueness of boot orders among disks and interfaces
	bootOrderMap := make(map[uint]bool)

	// Validate disks and volumes match up correctly
	for idx, disk := range spec.Domain.Devices.Disks {
		var matchingVolume *v1.Volume

		matchingVolume, volumeExists := volumeNameMap[disk.Name]

		if !volumeExists {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s '%s' not found.", field.Child("domain", "devices", "disks").Index(idx).Child("Name").String(), disk.Name),
				Field:   field.Child("domain", "devices", "disks").Index(idx).Child("name").String(),
			})
		}

		// Verify Lun disks are only mapped to network/block devices.
		if disk.LUN != nil && volumeExists && matchingVolume.PersistentVolumeClaim == nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s can only be mapped to a PersistentVolumeClaim volume.", field.Child("domain", "devices", "disks").Index(idx).Child("lun").String()),
				Field:   field.Child("domain", "devices", "disks").Index(idx).Child("lun").String(),
			})
		}

		// verify that there are no duplicate boot orders
		if disk.BootOrder != nil {
			order := *disk.BootOrder
			if bootOrderMap[order] {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("Boot order for %s already set for a different device.", field.Child("domain", "devices", "disks").Index(idx).Child("bootOrder").String()),
					Field:   field.Child("domain", "devices", "disks").Index(idx).Child("bootOrder").String(),
				})
			}
			bootOrderMap[order] = true
		}
	}

	if len(spec.Networks) > 0 && len(spec.Domain.Devices.Interfaces) > 0 {
		multusDefaultCount := 0
		multusExists := false
		genieExists := false
		podExists := false

		for idx, network := range spec.Networks {

			cniTypesCount := 0
			// network name not needed by default
			networkNameExistsOrNotNeeded := true

			if network.Pod != nil {
				cniTypesCount++
				podExists = true
			}

			if network.NetworkSource.Multus != nil {
				cniTypesCount++
				multusExists = true
				networkNameExistsOrNotNeeded = network.Multus.NetworkName != ""
				if network.NetworkSource.Multus.Default {
					multusDefaultCount++
				}
			}

			if network.NetworkSource.Genie != nil {
				cniTypesCount++
				genieExists = true
				networkNameExistsOrNotNeeded = network.Genie.NetworkName != ""
			}

			if cniTypesCount == 0 {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueRequired,
					Message: fmt.Sprintf("should have a network type"),
					Field:   field.Child("networks").Index(idx).String(),
				})
			} else if cniTypesCount > 1 {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueRequired,
					Message: fmt.Sprintf("should have only one network type"),
					Field:   field.Child("networks").Index(idx).String(),
				})
			} else if genieExists && (podExists || multusExists) {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueRequired,
					Message: fmt.Sprintf("cannot combine Genie with other CNIs across networks"),
					Field:   field.Child("networks").Index(idx).String(),
				})
			}

			if !networkNameExistsOrNotNeeded {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueRequired,
					Message: fmt.Sprintf("CNI delegating plugin must have a networkName"),
					Field:   field.Child("networks").Index(idx).String(),
				})
			}

			networkNameMap[spec.Networks[idx].Name] = &spec.Networks[idx]
		}

		if multusDefaultCount > 1 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("Multus CNI should only have one default network"),
				Field:   field.Child("networks").String(),
			})
		}

		if podExists && multusDefaultCount > 0 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("Pod network cannot be defined when Multus default network is defined"),
				Field:   field.Child("networks").String(),
			})
		}

		// Make sure interfaces and networks are 1to1 related
		networkInterfaceMap := make(map[string]struct{})

		// Make sure the port name is unique across all the interfaces
		portForwardMap := make(map[string]struct{})

		vifMQ := spec.Domain.Devices.NetworkInterfaceMultiQueue
		isVirtioNicRequested := false

		// Validate that each interface has a matching network
		for idx, iface := range spec.Domain.Devices.Interfaces {

			networkData, networkExists := networkNameMap[iface.Name]

			if !networkExists {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s '%s' not found.", field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(), iface.Name),
					Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(),
				})
			} else if iface.Slirp != nil && networkData.Pod == nil {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("Slirp interface only implemented with pod network"),
					Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(),
				})
			} else if iface.Slirp != nil && networkData.Pod != nil && !config.IsSlirpInterfaceEnabled() {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("Slirp interface is not enabled in kubevirt-config"),
					Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(),
				})
			} else if iface.Masquerade != nil && networkData.Pod == nil {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("Masquerade interface only implemented with pod network"),
					Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(),
				})
			}

			// Check if the interface name is unique
			if _, networkAlreadyUsed := networkInterfaceMap[iface.Name]; networkAlreadyUsed {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueDuplicate,
					Message: fmt.Sprintf("Only one interface can be connected to one specific network"),
					Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(),
				})
			}

			networkInterfaceMap[iface.Name] = struct{}{}

			// Check only ports configured on interfaces connected to a pod network
			if networkExists && networkData.Pod != nil && iface.Ports != nil {
				for portIdx, forwardPort := range iface.Ports {

					if forwardPort.Port == 0 {
						causes = append(causes, metav1.StatusCause{
							Type:    metav1.CauseTypeFieldValueRequired,
							Message: fmt.Sprintf("Port field is mandatory in every Port"),
							Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("ports").Index(portIdx).String(),
						})
					}

					if forwardPort.Port < 0 || forwardPort.Port > 65536 {
						causes = append(causes, metav1.StatusCause{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("Port field must be in range 0 < x < 65536."),
							Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("ports").Index(portIdx).String(),
						})
					}

					if forwardPort.Protocol != "" {
						if forwardPort.Protocol != "TCP" && forwardPort.Protocol != "UDP" {
							causes = append(causes, metav1.StatusCause{
								Type:    metav1.CauseTypeFieldValueInvalid,
								Message: fmt.Sprintf("Unknown protocol, only TCP or UDP allowed"),
								Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("ports").Index(portIdx).Child("protocol").String(),
							})
						}
					} else {
						forwardPort.Protocol = "TCP"
					}

					if forwardPort.Name != "" {
						if _, ok := portForwardMap[forwardPort.Name]; ok {
							causes = append(causes, metav1.StatusCause{
								Type:    metav1.CauseTypeFieldValueDuplicate,
								Message: fmt.Sprintf("Duplicate name of the port: %s", forwardPort.Name),
								Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("ports").Index(portIdx).Child("name").String(),
							})
						}

						if msgs := validation.IsValidPortName(forwardPort.Name); len(msgs) != 0 {
							causes = append(causes, metav1.StatusCause{
								Type:    metav1.CauseTypeFieldValueInvalid,
								Message: fmt.Sprintf("Invalid name of the port: %s", forwardPort.Name),
								Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("ports").Index(portIdx).Child("name").String(),
							})
						}

						portForwardMap[forwardPort.Name] = struct{}{}
					}
				}
			}

			// verify that selected model is supported
			if iface.Model != "" {
				isModelSupported := func(model string) bool {
					for _, m := range validInterfaceModels {
						if m == model {
							return true
						}
					}
					return false
				}
				if !isModelSupported(iface.Model) {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueNotSupported,
						Message: fmt.Sprintf("interface %s uses model %s that is not supported.", field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(), iface.Model),
						Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("model").String(),
					})
				}
			}

			// verify that selected macAddress is valid
			if iface.MacAddress != "" {
				mac, err := net.ParseMAC(iface.MacAddress)
				if err != nil {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("interface %s has malformed MAC address (%s).", field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(), iface.MacAddress),
						Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("macAddress").String(),
					})
				}
				if len(mac) > 6 {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("interface %s has MAC address (%s) that is too long.", field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(), iface.MacAddress),
						Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("macAddress").String(),
					})
				}
			}

			if iface.BootOrder != nil {
				order := *iface.BootOrder
				// Verify boot order is greater than 0, if provided
				if order < 1 {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("%s must have a boot order > 0, if supplied", field.Index(idx).String()),
						Field:   field.Index(idx).Child("bootOrder").String(),
					})
				} else {
					// verify that there are no duplicate boot orders
					if bootOrderMap[order] {
						causes = append(causes, metav1.StatusCause{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("Boot order for %s already set for a different device.", field.Child("domain", "devices", "interfaces").Index(idx).Child("bootOrder").String()),
							Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("bootOrder").String(),
						})
					}
					bootOrderMap[order] = true
				}
			}
			// verify that the specified pci address is valid
			if iface.PciAddress != "" {
				_, err := util.ParsePciAddress(iface.PciAddress)
				if err != nil {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("interface %s has malformed PCI address (%s).", field.Child("domain", "devices", "interfaces").Index(idx).Child("name").String(), iface.PciAddress),
						Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("pciAddress").String(),
					})
				}
			}
			// verify that the extra dhcp options are valid
			if iface.DHCPOptions != nil {
				PrivateOptions := iface.DHCPOptions.PrivateOptions
				err := ValidateDuplicateDHCPPrivateOptions(PrivateOptions)
				if err != nil {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("Found Duplicates: %v", err),
						Field:   field.String(),
					})
					return causes
				}
				for _, DHCPPrivateOption := range PrivateOptions {
					if !(DHCPPrivateOption.Option >= 224 && DHCPPrivateOption.Option <= 254) {
						causes = append(causes, metav1.StatusCause{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: "provided DHCPPrivateOptions are out of range, must be in range 224 to 254",
							Field:   field.String(),
						})
					}
				}
			}

			if iface.Model == "virtio" || iface.Model == "" {
				isVirtioNicRequested = true
			}

			if iface.DHCPOptions != nil {
				for index, ip := range iface.DHCPOptions.NTPServers {
					if net.ParseIP(ip).To4() == nil {
						causes = append(causes, metav1.StatusCause{
							Type:    metav1.CauseTypeFieldValueInvalid,
							Message: fmt.Sprintf("NTP servers must be a valid IPv4 address."),
							Field:   field.Child("domain", "devices", "interfaces").Index(idx).Child("dhcpOptions", "ntpServers").Index(index).String(),
						})
					}
				}
			}
		}
		// Network interface multiqueue can only be set for a virtio driver
		if vifMQ != nil && *vifMQ && !isVirtioNicRequested {

			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("virtio-net multiqueue request, but there are no virtio interfaces defined"),
				Field:   field.Child("domain", "devices", "networkInterfaceMultiqueue").String(),
			})

		}

		// Validate that every network was assign to an interface
		if len(networkInterfaceMap) != len(networkNameMap) {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueRequired,
				Message: fmt.Sprintf("every network must be mapped to an interface."),
				Field:   field.Child("networks").String(),
			})
		}
	}

	for idx, input := range spec.Domain.Devices.Inputs {
		if input.Bus != "virtio" && input.Bus != "usb" && input.Bus != "" {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("Input device can have only virtio or usb bus."),
				Field:   field.Child("domain", "devices", "inputs").Index(idx).Child("bus").String(),
			})
		}

		if input.Type != "tablet" {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("Input device can have only tablet type."),
				Field:   field.Child("domain", "devices", "inputs").Index(idx).Child("type").String(),
			})
		}
	}
	_, requestOk := spec.Domain.Resources.Requests[k8sv1.ResourceCPU]
	_, limitOK := spec.Domain.Resources.Limits[k8sv1.ResourceCPU]
	isCPUResourcesSet := (requestOk == true) || (limitOK == true)
	if !isCPUResourcesSet && (spec.Domain.Devices.BlockMultiQueue != nil) && (*spec.Domain.Devices.BlockMultiQueue == true) {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("MultiQueue for block devices can't be used without specifying CPU requests or limits."),
			Field:   field.Child("domain", "devices", "blockMultiQueue").String(),
		})
	}
	if !isCPUResourcesSet && (spec.Domain.Devices.NetworkInterfaceMultiQueue != nil) && (*spec.Domain.Devices.NetworkInterfaceMultiQueue == true) {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("MultiQueue for network interfaces can't be used without specifying CPU requests or limits."),
			Field:   field.Child("domain", "devices", "networkInterfaceMultiqueue").String(),
		})
	}

	if spec.Domain.IOThreadsPolicy != nil {
		isValidPolicy := func(policy v1.IOThreadsPolicy) bool {
			for _, p := range validIOThreadsPolicies {
				if policy == p {
					return true
				}
			}
			return false
		}
		if !isValidPolicy(*spec.Domain.IOThreadsPolicy) {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("Invalid IOThreadsPolicy (%s)", *spec.Domain.IOThreadsPolicy),
				Field:   field.Child("domain", "ioThreadsPolicy").String(),
			})
		}
	}

	if spec.ReadinessProbe != nil {
		if spec.ReadinessProbe.HTTPGet != nil && spec.ReadinessProbe.TCPSocket != nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must have exactly one probe type set", field.Child("readinessProbe").String()),
				Field:   field.Child("readinessProbe").String(),
			})
		} else if spec.ReadinessProbe.HTTPGet == nil && spec.ReadinessProbe.TCPSocket == nil {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueRequired,
				Message: fmt.Sprintf("either %s or %s must be set if a %s is specified",
					field.Child("readinessProbe", "tcpSocket").String(),
					field.Child("readinessProbe", "httpGet").String(),
					field.Child("readinessProbe").String(),
				),
				Field: field.Child("readinessProbe").String(),
			})
		}
	}

	if spec.LivenessProbe != nil {
		if spec.LivenessProbe.HTTPGet != nil && spec.LivenessProbe.TCPSocket != nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must have exactly one probe type set", field.Child("livenessProbe").String()),
				Field:   field.Child("livenessProbe").String(),
			})
		} else if spec.LivenessProbe.HTTPGet == nil && spec.LivenessProbe.TCPSocket == nil {
			causes = append(causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueRequired,
				Message: fmt.Sprintf("either %s or %s must be set if a %s is specified",
					field.Child("livenessProbe", "tcpSocket").String(),
					field.Child("livenessProbe", "httpGet").String(),
					field.Child("livenessProbe").String(),
				),
				Field: field.Child("livenessProbe").String(),
			})
		}
	}

	if !podNetworkInterfacePresent {
		if spec.LivenessProbe != nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s is only allowed if the Pod Network is attached", field.Child("livenessProbe").String()),
				Field:   field.Child("livenessProbe").String(),
			})
		}
		if spec.ReadinessProbe != nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s is only allowed if the Pod Network is attached", field.Child("readinessProbe").String()),
				Field:   field.Child("readinessProbe").String(),
			})
		}
	}

	causes = append(causes, validateDomainSpec(field.Child("domain"), &spec.Domain)...)
	causes = append(causes, validateVolumes(field.Child("volumes"), spec.Volumes, config)...)
	if spec.DNSPolicy != "" {
		causes = append(causes, validateDNSPolicy(&spec.DNSPolicy, field.Child("dnsPolicy"))...)
	}
	causes = append(causes, validatePodDNSConfig(spec.DNSConfig, &spec.DNSPolicy, field.Child("dnsConfig"))...)

	if !config.LiveMigrationEnabled() && spec.EvictionStrategy != nil {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: "LiveMigration feature gate is not enabled",
			Field:   field.Child("evictionStrategy").String(),
		})
	} else if spec.EvictionStrategy != nil {
		if *spec.EvictionStrategy != v1.EvictionStrategyLiveMigrate {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s is set with an unrecognized option: %s", field.Child("evictionStrategy").String(), *spec.EvictionStrategy),
				Field:   field.Child("evictionStrategy").String(),
			})
		}

	}

	return causes
}

// ValidateVirtualMachineInstanceMandatoryFields should be invoked after all defaults and presets are applied.
// It is only meant to be used for VMI reviews, not if they are templates on other objects
func ValidateVirtualMachineInstanceMandatoryFields(field *k8sfield.Path, spec *v1.VirtualMachineInstanceSpec) []metav1.StatusCause {
	var causes []metav1.StatusCause

	requests := spec.Domain.Resources.Requests.Memory().Value()

	if requests == 0 &&
		(spec.Domain.Memory == nil || spec.Domain.Memory != nil &&
			spec.Domain.Memory.Guest == nil && spec.Domain.Memory.Hugepages == nil) {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueRequired,
			Message: fmt.Sprintf("no memory requested, at least one of '%s', '%s' or '%s' must be set",
				field.Child("domain", "memory", "guest").String(),
				field.Child("domain", "memory", "hugepages", "size").String(),
				field.Child("domain", "resources", "requests", "memory").String()),
		})
	}
	return causes
}

func ValidateVirtualMachineInstanceMetadata(field *k8sfield.Path, metadata *metav1.ObjectMeta, config *virtconfig.ClusterConfig) []metav1.StatusCause {
	var causes []metav1.StatusCause
	annotations := metadata.Annotations

	// Validate ignition feature gate if set when the corresponding annotation is found
	if annotations[v1.IgnitionAnnotation] != "" && !config.IgnitionEnabled() {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("ExperimentalIgnitionSupport feature gate is not enabled in kubevirt-config, invalid entry %s",
				field.Child("annotations").Child(v1.IgnitionAnnotation).String()),
			Field: field.Child("annotations").String(),
		})
	}

	// Validate sidecar feature gate if set when the corresponding annotation is found
	if annotations[hooks.HookSidecarListAnnotationName] != "" && !config.SidecarEnabled() {
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("sidecar feature gate is not enabled in kubevirt-config, invalid entry %s",
				field.Child("annotations", hooks.HookSidecarListAnnotationName).String()),
			Field: field.Child("annotations").String(),
		})
	}

	return causes
}

func getAdmissionReviewVMI(ar *v1beta1.AdmissionReview) (new *v1.VirtualMachineInstance, old *v1.VirtualMachineInstance, err error) {

	if !webhooks.ValidateRequestResource(ar.Request.Resource, webhooks.VirtualMachineInstanceGroupVersionResource.Group, webhooks.VirtualMachineInstanceGroupVersionResource.Resource) {
		return nil, nil, fmt.Errorf("expect resource to be '%s'", webhooks.VirtualMachineInstanceGroupVersionResource.Resource)
	}

	raw := ar.Request.Object.Raw
	newVMI := v1.VirtualMachineInstance{}

	err = json.Unmarshal(raw, &newVMI)
	if err != nil {
		return nil, nil, err
	}

	if ar.Request.Operation == v1beta1.Update {
		raw := ar.Request.OldObject.Raw
		oldVMI := v1.VirtualMachineInstance{}

		err = json.Unmarshal(raw, &oldVMI)
		if err != nil {
			return nil, nil, err
		}
		return &newVMI, &oldVMI, nil
	}

	return &newVMI, nil, nil
}

func ValidateDuplicateDHCPPrivateOptions(PrivateOptions []v1.DHCPPrivateOptions) error {
	isUnique := map[int]bool{}
	for _, DHCPPrivateOption := range PrivateOptions {
		if isUnique[DHCPPrivateOption.Option] == true {
			return fmt.Errorf("You have provided duplicate DHCPPrivateOptions")
		}
		isUnique[DHCPPrivateOption.Option] = true
	}
	return nil
}

// Copied from kubernetes/pkg/apis/core/validation/validation.go
func validatePodDNSConfig(dnsConfig *k8sv1.PodDNSConfig, dnsPolicy *k8sv1.DNSPolicy, field *k8sfield.Path) []metav1.StatusCause {
	var causes []metav1.StatusCause

	// Validate DNSNone case. Must provide at least one DNS name server.
	if dnsPolicy != nil && *dnsPolicy == k8sv1.DNSNone {
		if dnsConfig == nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueRequired,
				Message: fmt.Sprintf("must provide `dnsConfig` when `dnsPolicy` is %s", k8sv1.DNSNone),
				Field:   field.String(),
			})
			return causes
		}
		if len(dnsConfig.Nameservers) == 0 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueRequired,
				Message: fmt.Sprintf("must provide at least one DNS nameserver when `dnsPolicy` is %s", k8sv1.DNSNone),
				Field:   "nameservers",
			})
			return causes
		}
	}

	if dnsConfig != nil {
		// Validate nameservers.
		if len(dnsConfig.Nameservers) > maxDNSNameservers {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("must not have more than %v nameservers: %s", maxDNSNameservers, dnsConfig.Nameservers),
				Field:   "nameservers",
			})
		}
		for _, ns := range dnsConfig.Nameservers {
			if ip := net.ParseIP(ns); ip == nil {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("must be valid IP address: %s", ns),
					Field:   "nameservers",
				})
			}
		}
		// Validate searches.
		if len(dnsConfig.Searches) > maxDNSSearchPaths {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("must not have more than %v search paths", maxDNSSearchPaths),
				Field:   "searchDomains",
			})
		}
		// Include the space between search paths.
		if len(strings.Join(dnsConfig.Searches, " ")) > maxDNSSearchListChars {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("must not have more than %v characters (including spaces) in the search list", maxDNSSearchListChars),
				Field:   "searchDomains",
			})
		}
		for _, search := range dnsConfig.Searches {
			for _, msg := range validation.IsDNS1123Subdomain(search) {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%v", msg),
					Field:   "searchDomains",
				})
			}
		}
		// Validate options.
		for _, option := range dnsConfig.Options {
			if len(option.Name) == 0 {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("Option.Name must not be empty for value: %s", *option.Value),
					Field:   "options",
				})
			}
		}
	}
	return causes
}

// Copied from kubernetes/pkg/apis/core/validation/validation.go
func validateDNSPolicy(dnsPolicy *k8sv1.DNSPolicy, field *k8sfield.Path) []metav1.StatusCause {
	var causes []metav1.StatusCause
	switch *dnsPolicy {
	case k8sv1.DNSClusterFirstWithHostNet, k8sv1.DNSClusterFirst, k8sv1.DNSDefault, k8sv1.DNSNone, "":
	default:
		validValues := []string{string(k8sv1.DNSClusterFirstWithHostNet), string(k8sv1.DNSClusterFirst), string(k8sv1.DNSDefault), string(k8sv1.DNSNone), ""}
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueNotSupported,
			Message: fmt.Sprintf("DNSPolicy: %s is not supported, valid values: %s", *dnsPolicy, validValues),
			Field:   field.String(),
		})
	}
	return causes
}

func validateBootloader(field *k8sfield.Path, bootloader *v1.Bootloader) []metav1.StatusCause {
	var causes []metav1.StatusCause

	if bootloader != nil && bootloader.EFI != nil && bootloader.BIOS != nil {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s has both EFI and BIOS configured, but they are mutually exclusive.", field.String()),
			Field:   field.String(),
		})
	}

	return causes
}

func validateFirmware(field *k8sfield.Path, firmware *v1.Firmware) []metav1.StatusCause {
	var causes []metav1.StatusCause

	if firmware != nil {
		causes = append(causes, validateBootloader(field.Child("bootloader"), firmware.Bootloader)...)
	}

	return causes
}

func validateDomainSpec(field *k8sfield.Path, spec *v1.DomainSpec) []metav1.StatusCause {
	var causes []metav1.StatusCause
	causes = append(causes, validateDevices(field.Child("devices"), &spec.Devices)...)
	causes = append(causes, validateFirmware(field.Child("firmware"), spec.Firmware)...)
	return causes
}

func validateVolumes(field *k8sfield.Path, volumes []v1.Volume, config *virtconfig.ClusterConfig) []metav1.StatusCause {
	var causes []metav1.StatusCause
	nameMap := make(map[string]int)

	if len(volumes) > arrayLenMax {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s list exceeds the %d element limit in length", field.String(), arrayLenMax),
			Field:   field.String(),
		})
		// We won't process anything over the limit
		return causes
	}

	// check that we have max 1 serviceAccount volume
	serviceAccountVolumeCount := 0

	for idx, volume := range volumes {
		// verify name is unique
		otherIdx, ok := nameMap[volume.Name]
		if !ok {
			nameMap[volume.Name] = idx
		} else {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s and %s must not have the same Name.", field.Index(idx).String(), field.Index(otherIdx).String()),
				Field:   field.Index(idx).Child("name").String(),
			})
		}

		// Verify exactly one source is set
		volumeSourceSetCount := 0
		if volume.PersistentVolumeClaim != nil {
			volumeSourceSetCount++
		}
		if volume.CloudInitNoCloud != nil {
			volumeSourceSetCount++
		}
		if volume.CloudInitConfigDrive != nil {
			volumeSourceSetCount++
		}
		if volume.ContainerDisk != nil {
			volumeSourceSetCount++
		}
		if volume.Ephemeral != nil {
			volumeSourceSetCount++
		}
		if volume.EmptyDisk != nil {
			volumeSourceSetCount++
		}
		if volume.HostDisk != nil {
			volumeSourceSetCount++
		}
		if volume.DataVolume != nil {
			if !config.HasDataVolumeAPI() {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: "DataVolume api is not present in cluster. CDI must be installed for DataVolume support.",
					Field:   field.Index(idx).String(),
				})
			}

			if volume.DataVolume.Name == "" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueRequired,
					Message: "DataVolume 'name' must be set",
					Field:   field.Index(idx).Child("name").String(),
				})
			}
			volumeSourceSetCount++
		}
		if volume.ConfigMap != nil {
			volumeSourceSetCount++
		}
		if volume.Secret != nil {
			volumeSourceSetCount++
		}
		if volume.ServiceAccount != nil {
			volumeSourceSetCount++
			serviceAccountVolumeCount++
		}

		if volumeSourceSetCount != 1 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must have exactly one source type set", field.Index(idx).String()),
				Field:   field.Index(idx).String(),
			})
		}

		// Verify cloud init data is within size limits
		if volume.CloudInitNoCloud != nil || volume.CloudInitConfigDrive != nil {
			var userDataSecretRef, networkDataSecretRef *k8sv1.LocalObjectReference
			var dataSourceType, userData, userDataBase64, networkData, networkDataBase64 string
			if volume.CloudInitNoCloud != nil {
				dataSourceType = "cloudInitNoCloud"
				userDataSecretRef = volume.CloudInitNoCloud.UserDataSecretRef
				userDataBase64 = volume.CloudInitNoCloud.UserDataBase64
				userData = volume.CloudInitNoCloud.UserData
				networkDataSecretRef = volume.CloudInitNoCloud.NetworkDataSecretRef
				networkDataBase64 = volume.CloudInitNoCloud.NetworkDataBase64
				networkData = volume.CloudInitNoCloud.NetworkData
			} else if volume.CloudInitConfigDrive != nil {
				dataSourceType = "cloudInitConfigDrive"
				userDataSecretRef = volume.CloudInitConfigDrive.UserDataSecretRef
				userDataBase64 = volume.CloudInitConfigDrive.UserDataBase64
				userData = volume.CloudInitConfigDrive.UserData
				networkDataSecretRef = volume.CloudInitConfigDrive.NetworkDataSecretRef
				networkDataBase64 = volume.CloudInitConfigDrive.NetworkDataBase64
				networkData = volume.CloudInitConfigDrive.NetworkData
			}

			userDataLen := 0
			userDataSourceCount := 0
			networkDataLen := 0
			networkDataSourceCount := 0

			if userDataSecretRef != nil && userDataSecretRef.Name != "" {
				userDataSourceCount++
			}
			if userDataBase64 != "" {
				userDataSourceCount++
				userData, err := base64.StdEncoding.DecodeString(userDataBase64)
				if err != nil {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("%s.%s.userDataBase64 is not a valid base64 value.", field.Index(idx).Child(dataSourceType, "userDataBase64").String(), dataSourceType),
						Field:   field.Index(idx).Child(dataSourceType, "userDataBase64").String(),
					})
				}
				userDataLen = len(userData)
			}
			if userData != "" {
				userDataSourceCount++
				userDataLen = len(userData)
			}

			if userDataSourceCount != 1 {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s must have one exactly one userdata source set.", field.Index(idx).Child(dataSourceType).String()),
					Field:   field.Index(idx).Child(dataSourceType).String(),
				})
			}

			if userDataLen > cloudInitUserMaxLen {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s userdata exceeds %d byte limit. Should use UserDataSecretRef for larger data.", field.Index(idx).Child(dataSourceType).String(), cloudInitUserMaxLen),
					Field:   field.Index(idx).Child(dataSourceType).String(),
				})
			}

			if networkDataSecretRef != nil && networkDataSecretRef.Name != "" {
				networkDataSourceCount++
			}
			if networkDataBase64 != "" {
				networkDataSourceCount++
				networkData, err := base64.StdEncoding.DecodeString(networkDataBase64)
				if err != nil {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("%s.%s.networkDataBase64 is not a valid base64 value.", field.Index(idx).Child(dataSourceType, "networkDataBase64").String(), dataSourceType),
						Field:   field.Index(idx).Child(dataSourceType, "networkDataBase64").String(),
					})
				}
				networkDataLen = len(networkData)
			}
			if networkData != "" {
				networkDataSourceCount++
				networkDataLen = len(networkData)
			}

			if networkDataSourceCount > 1 {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s must have only one networkdata source set.", field.Index(idx).Child(dataSourceType).String()),
					Field:   field.Index(idx).Child(dataSourceType).String(),
				})
			}

			if networkDataLen > cloudInitNetworkMaxLen {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s networkdata exceeds %d byte limit. Should use NetworkDataSecretRef for larger data.", field.Index(idx).Child(dataSourceType).String(), cloudInitNetworkMaxLen),
					Field:   field.Index(idx).Child(dataSourceType).String(),
				})
			}
		}

		// validate HostDisk data
		if hostDisk := volume.HostDisk; hostDisk != nil {
			if hostDisk.Path == "" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueNotFound,
					Message: fmt.Sprintf("%s is required for hostDisk volume", field.Index(idx).Child("hostDisk", "path").String()),
					Field:   field.Index(idx).Child("hostDisk", "path").String(),
				})
			}

			if hostDisk.Type != v1.HostDiskExists && hostDisk.Type != v1.HostDiskExistsOrCreate {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s has invalid value '%s', allowed are '%s' or '%s'", field.Index(idx).Child("hostDisk", "type").String(), hostDisk.Type, v1.HostDiskExists, v1.HostDiskExistsOrCreate),
					Field:   field.Index(idx).Child("hostDisk", "type").String(),
				})
			}

			// if disk.img already exists and user knows that by specifying type 'Disk' it is pointless to set capacity
			if hostDisk.Type == v1.HostDiskExists && !hostDisk.Capacity.IsZero() {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s is allowed to pass only with %s equal to '%s'", field.Index(idx).Child("hostDisk", "capacity").String(), field.Index(idx).Child("hostDisk", "type").String(), v1.HostDiskExistsOrCreate),
					Field:   field.Index(idx).Child("hostDisk", "capacity").String(),
				})
			}
		}

		if volume.ConfigMap != nil {
			if volume.ConfigMap.LocalObjectReference.Name == "" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s is a required field", field.Index(idx).Child("configMap", "name").String()),
					Field:   field.Index(idx).Child("configMap", "name").String(),
				})
			}
		}

		if volume.Secret != nil {
			if volume.Secret.SecretName == "" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s is a required field", field.Index(idx).Child("secret", "secretName").String()),
					Field:   field.Index(idx).Child("secret", "secretName").String(),
				})
			}
		}

		if volume.ServiceAccount != nil {
			if volume.ServiceAccount.ServiceAccountName == "" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("%s is a required field", field.Index(idx).Child("serviceAccount", "serviceAccountName").String()),
					Field:   field.Index(idx).Child("serviceAccount", "serviceAccountName").String(),
				})
			}
		}
	}

	if serviceAccountVolumeCount > 1 {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s must have max one serviceAccount volume set", field.String()),
			Field:   field.String(),
		})
	}

	return causes
}

func validateDevices(field *k8sfield.Path, devices *v1.Devices) []metav1.StatusCause {
	var causes []metav1.StatusCause
	causes = append(causes, validateDisks(field.Child("disks"), devices.Disks)...)
	return causes
}

func getNumberOfPodInterfaces(spec *v1.VirtualMachineInstanceSpec) int {
	nPodInterfaces := 0
	for _, net := range spec.Networks {
		if net.Pod != nil {
			for _, iface := range spec.Domain.Devices.Interfaces {
				if iface.Name == net.Name {
					nPodInterfaces++
					break // we maintain 1-to-1 relationship between networks and interfaces
				}
			}
		}
	}
	return nPodInterfaces
}

func validateDisks(field *k8sfield.Path, disks []v1.Disk) []metav1.StatusCause {
	var causes []metav1.StatusCause
	nameMap := make(map[string]int)

	if len(disks) > arrayLenMax {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("%s list exceeds the %d element limit in length", field.String(), arrayLenMax),
			Field:   field.String(),
		})
		// We won't process anything over the limit
		return causes
	}

	for idx, disk := range disks {
		// verify name is unique
		otherIdx, ok := nameMap[disk.Name]
		if !ok {
			nameMap[disk.Name] = idx
		} else {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s and %s must not have the same Name.", field.Index(idx).String(), field.Index(otherIdx).String()),
				Field:   field.Index(idx).Child("name").String(),
			})
		}

		// Reject Floppy disks
		if disk.Floppy != nil {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueNotSupported,
				Message: fmt.Sprintf("Floppy disks are deprecated and will be removed from the API soon."),
				Field:   field.Index(idx).Child("name").String(),
			})
		}

		// Verify only a single device type is set.
		deviceTargetSetCount := 0
		var diskType, bus string
		if disk.Disk != nil {
			deviceTargetSetCount++
			diskType = "disk"
			bus = disk.Disk.Bus
		}
		if disk.LUN != nil {
			deviceTargetSetCount++
			diskType = "lun"
			bus = disk.LUN.Bus
		}
		if disk.Floppy != nil {
			deviceTargetSetCount++
		}
		if disk.CDRom != nil {
			deviceTargetSetCount++
			diskType = "cdrom"
			bus = disk.CDRom.Bus
		}

		// NOTE: not setting a device target is okay. We default to Disk.
		// However, only a single device target is allowed to be set at a time.
		if deviceTargetSetCount > 1 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s can only have a single target type defined", field.Index(idx).String()),
				Field:   field.Index(idx).String(),
			})
		}

		// Verify pci address
		if disk.Disk != nil && disk.Disk.PciAddress != "" {
			if disk.Disk.Bus != "virtio" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("disk %s - setting a PCI address is only possible with bus type virtio.", field.Child("domain", "devices", "disks", "disk").Index(idx).Child("name").String()),
					Field:   field.Child("domain", "devices", "disks", "disk").Index(idx).Child("pciAddress").String(),
				})
			}

			_, err := util.ParsePciAddress(disk.Disk.PciAddress)
			if err != nil {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: fmt.Sprintf("disk %s has malformed PCI address (%s).", field.Child("domain", "devices", "disks", "disk").Index(idx).Child("name").String(), disk.Disk.PciAddress),
					Field:   field.Child("domain", "devices", "disks", "disk").Index(idx).Child("pciAddress").String(),
				})
			}
		}

		// Verify boot order is greater than 0, if provided
		if disk.BootOrder != nil && *disk.BootOrder < 1 {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must have a boot order > 0, if supplied", field.Index(idx).String()),
				Field:   field.Index(idx).Child("bootOrder").String(),
			})
		}

		// Verify bus is supported, if provided
		if len(bus) > 0 {
			if bus == "ide" {
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Message: "IDE bus is not supported",
					Field:   field.Index(idx).Child(diskType, "bus").String(),
				})
			} else {
				buses := []string{"virtio", "sata", "scsi"}
				validBus := false
				for _, b := range buses {
					if b == bus {
						validBus = true
					}
				}
				if !validBus {
					causes = append(causes, metav1.StatusCause{
						Type:    metav1.CauseTypeFieldValueInvalid,
						Message: fmt.Sprintf("%s is set with an unrecognized bus %s, must be one of: %v", field.Index(idx).String(), bus, buses),
						Field:   field.Index(idx).Child(diskType, "bus").String(),
					})
				}
			}
		}

		// Verify serial number is made up of valid characters for libvirt, if provided
		isValid := regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`).MatchString
		if disk.Serial != "" && !isValid(disk.Serial) {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must be made up of the following characters [A-Za-z0-9_.+-], if specified", field.Index(idx).String()),
				Field:   field.Index(idx).Child("serial").String(),
			})
		}

		// Verify serial number is within valid length, if provided
		if disk.Serial != "" && len([]rune(disk.Serial)) > maxStrLen {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s must be less than or equal to %d in length, if specified", field.Index(idx).String(), maxStrLen),
				Field:   field.Index(idx).Child("serial").String(),
			})
		}

		// Verify if cache mode is valid
		if disk.Cache != "" && disk.Cache != v1.CacheNone && disk.Cache != v1.CacheWriteThrough {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fmt.Sprintf("%s has invalid value %s", field.Index(idx).Child("cache").String(), disk.Cache),
				Field:   field.Index(idx).Child("cache").String(),
			})
		}

		// Verify disk and volume name can be a valid container name since disk
		// name can become a container name which will fail to schedule if invalid
		errs := validation.IsDNS1123Label(disk.Name)

		for _, err := range errs {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: err,
				Field:   field.Child("domain", "devices", "disks").Index(idx).Child("name").String(),
			})
		}
	}

	return causes
}
