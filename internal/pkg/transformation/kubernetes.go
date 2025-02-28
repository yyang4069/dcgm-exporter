/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
 */

package transformation

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/resolver"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1alpha1"

	"github.com/NVIDIA/dcgm-exporter/internal/pkg/appconfig"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/collector"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/deviceinfo"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/nvmlprovider"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/utils"
)

var (
	connectionTimeout = 10 * time.Second

	// Allow for MIG devices with or without GPU sharing to match in GKE.
	gkeMigDeviceIDRegex            = regexp.MustCompile(`^nvidia([0-9]+)/gi([0-9]+)(/vgpu[0-9]+)?$`)
	gkeVirtualGPUDeviceIDSeparator = "/vgpu"
)

func NewPodMapper(c *appconfig.Config) *PodMapper {
	slog.Info("Kubernetes metrics collection enabled!")

	return &PodMapper{
		Config: c,
	}
}

func (p *PodMapper) Name() string {
	return "podMapper"
}

func (p *PodMapper) Process(metrics collector.MetricsByCounter, deviceInfo deviceinfo.Provider) error {
	socketPath := p.Config.PodResourcesKubeletSocket
	_, err := os.Stat(socketPath)
	if os.IsNotExist(err) {
		slog.Info("No Kubelet socket, ignoring")
		return nil
	}

	// TODO: This needs to be moved out of the critical path.
	c, cleanup, err := connectToServer(socketPath)
	if err != nil {
		return err
	}
	defer cleanup()

	pods, err := p.listPods(c)
	if err != nil {
		return err
	}

	slog.Debug(fmt.Sprintf("Podresources API response: %+v", pods))
	if p.Config.KubernetesVirtualGPUs {
		deviceToPods := p.toDeviceToSharingPods(pods, deviceInfo)

		slog.Debug(fmt.Sprintf("Device to sharing pods mapping: %+v", deviceToPods))

		// For each counter metric, init a slice to collect metrics to associate with shared virtual GPUs.
		for counter := range metrics {
			var newmetrics []collector.Metric
			// For each instrumented device, build list of metrics and create
			// new metrics for any shared GPUs.
			for j, val := range metrics[counter] {
				deviceID, err := val.GetIDOfType(p.Config.KubernetesGPUIdType)
				if err != nil {
					return err
				}

				podInfos := deviceToPods[deviceID]
				// For all containers using the GPU, extract and annotate a metric
				// with the container info and the shared GPU label, if it exists.
				// Notably, this will increase the number of unique metrics (i.e. labelsets)
				// to by the number of containers sharing the GPU.
				for _, pi := range podInfos {
					metric, err := utils.DeepCopy(metrics[counter][j])
					if err != nil {
						return err
					}
					if !p.Config.UseOldNamespace {
						metric.Attributes[podAttribute] = pi.Name
						metric.Attributes[namespaceAttribute] = pi.Namespace
						metric.Attributes[containerAttribute] = pi.Container
					} else {
						metric.Attributes[oldPodAttribute] = pi.Name
						metric.Attributes[oldNamespaceAttribute] = pi.Namespace
						metric.Attributes[oldContainerAttribute] = pi.Container
					}
					if pi.VGPU != "" {
						metric.Attributes[vgpuAttribute] = pi.VGPU
					}
					newmetrics = append(newmetrics, metric)
				}
			}
			// Upsert the annotated metrics into the final map.
			metrics[counter] = newmetrics
		}
		return nil
	}

	deviceToPod := p.toDeviceToPod(pods, deviceInfo)

	slog.Debug(fmt.Sprintf("Device to pod mapping: %+v", deviceToPod))

	// Note: for loop are copies the value, if we want to change the value
	// and not the copy, we need to use the indexes
	for counter := range metrics {
		for j, val := range metrics[counter] {
			deviceID, err := val.GetIDOfType(p.Config.KubernetesGPUIdType)
			if err != nil {
				return err
			}

			podInfo, exists := deviceToPod[deviceID]
			if exists {
				if !p.Config.UseOldNamespace {
					metrics[counter][j].Attributes[podAttribute] = podInfo.Name
					metrics[counter][j].Attributes[namespaceAttribute] = podInfo.Namespace
					metrics[counter][j].Attributes[containerAttribute] = podInfo.Container
				} else {
					metrics[counter][j].Attributes[oldPodAttribute] = podInfo.Name
					metrics[counter][j].Attributes[oldNamespaceAttribute] = podInfo.Namespace
					metrics[counter][j].Attributes[oldContainerAttribute] = podInfo.Container
				}
				// hack the gpu use  idx in the container of pod idx not in the host
				metrics[counter][j].GPU = podInfo.GPU
			}
		}
	}

	return nil
}

func connectToServer(socket string) (*grpc.ClientConn, func(), error) {
	resolver.SetDefaultScheme("passthrough")
	conn, err := grpc.NewClient(
		socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", addr)
		}),
	)
	if err != nil {
		return nil, doNothing, fmt.Errorf("failure connecting to '%s'; err: %w", socket, err)
	}

	return conn, func() { conn.Close() }, nil
}

func (p *PodMapper) listPods(conn *grpc.ClientConn) (*podresourcesapi.ListPodResourcesResponse, error) {
	client := podresourcesapi.NewPodResourcesListerClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	resp, err := client.List(ctx, &podresourcesapi.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("failure getting pod resources; err: %w", err)
	}

	return resp, nil
}

// getSharedGPU parses the provided device ID and extracts the shared
// GPU identifier along with a boolean indicating if an identifier was
// found.
func getSharedGPU(deviceID string) (string, bool) {
	// Check if we're using the GKE device plugin or NVIDIA device plugin.
	if strings.Contains(deviceID, gkeVirtualGPUDeviceIDSeparator) {
		return strings.Split(deviceID, gkeVirtualGPUDeviceIDSeparator)[1], true
	} else if strings.Contains(deviceID, "::") {
		return strings.Split(deviceID, "::")[1], true
	}
	return "", false
}

// toDeviceToSharingPods uses the same general logic as toDeviceToPod but
// allows for multiple containers to be associated with a metric when sharing
// strategies are used in Kubernetes.
// TODO(pintohuch): the logic is manually duplicated from toDeviceToPod for
// better isolation and easier review. Ultimately, this logic should be
// merged into a single function that can handle both shared and non-shared
// GPU states.
func (p *PodMapper) toDeviceToSharingPods(devicePods *podresourcesapi.ListPodResourcesResponse, deviceInfo deviceinfo.Provider) map[string][]PodInfo {
	deviceToPodsMap := make(map[string][]PodInfo)

	for _, pod := range devicePods.GetPodResources() {
		for _, container := range pod.GetContainers() {
			for _, device := range container.GetDevices() {

				resourceName := device.GetResourceName()
				if resourceName != appconfig.NvidiaResourceName && !slices.Contains(p.Config.NvidiaResourceNames, resourceName) {
					// Mig resources appear differently than GPU resources
					if !strings.HasPrefix(resourceName, appconfig.NvidiaMigResourcePrefix) {
						continue
					}
				}

				podInfo := PodInfo{
					Name:      pod.GetName(),
					Namespace: pod.GetNamespace(),
					Container: container.GetName(),
				}

				for _, deviceID := range device.GetDeviceIds() {
					if vgpu, ok := getSharedGPU(deviceID); ok {
						podInfo.VGPU = vgpu
					}
					if strings.HasPrefix(deviceID, appconfig.MIG_UUID_PREFIX) {
						migDevice, err := nvmlprovider.Client().GetMIGDeviceInfoByID(deviceID)
						if err == nil {
							giIdentifier := deviceinfo.GetGPUInstanceIdentifier(deviceInfo, migDevice.ParentUUID,
								uint(migDevice.GPUInstanceID))
							deviceToPodsMap[giIdentifier] = append(deviceToPodsMap[giIdentifier], podInfo)
						}
						gpuUUID := deviceID[len(appconfig.MIG_UUID_PREFIX):]
						deviceToPodsMap[gpuUUID] = append(deviceToPodsMap[gpuUUID], podInfo)
					} else if gkeMigDeviceIDMatches := gkeMigDeviceIDRegex.FindStringSubmatch(deviceID); gkeMigDeviceIDMatches != nil {
						var gpuIndex string
						var gpuInstanceID string
						for groupIdx, group := range gkeMigDeviceIDMatches {
							switch groupIdx {
							case 1:
								gpuIndex = group
							case 2:
								gpuInstanceID = group
							}
						}
						giIdentifier := fmt.Sprintf("%s-%s", gpuIndex, gpuInstanceID)
						deviceToPodsMap[giIdentifier] = append(deviceToPodsMap[giIdentifier], podInfo)
					} else if strings.Contains(deviceID, gkeVirtualGPUDeviceIDSeparator) {
						deviceToPodsMap[strings.Split(deviceID, gkeVirtualGPUDeviceIDSeparator)[0]] = append(deviceToPodsMap[strings.Split(deviceID, gkeVirtualGPUDeviceIDSeparator)[0]], podInfo)
					} else if strings.Contains(deviceID, "::") {
						gpuInstanceID := strings.Split(deviceID, "::")[0]
						deviceToPodsMap[gpuInstanceID] = append(deviceToPodsMap[gpuInstanceID], podInfo)
					}
					// Default mapping between deviceID and pod information
					deviceToPodsMap[deviceID] = append(deviceToPodsMap[deviceID], podInfo)
				}
			}
		}
	}

	gpuIndexMap := make(map[string]int)
	for i, gpu := range deviceInfo.GPUs() {
		gpuIndexMap[gpu.DeviceInfo.UUID] = i
	}
	// add the index of the GPU in the pod
	podGpusMap := make(map[string][]string)
	for deviceID, podInfos := range deviceToPodsMap {
		gpuIndex, ok := gpuIndexMap[deviceID]

		if !ok {
			slog.Error("GPU not found in deviceInfo", "deviceID", deviceID, "gpuIndexMap", gpuIndexMap)
			continue
		}
		for _, podInfo := range podInfos {

			key := fmt.Sprintf("%s-%s-%s", podInfo.Namespace, podInfo.Name, podInfo.Container)
			id_device := fmt.Sprintf("%02d_%s", gpuIndex, deviceID)

			devicelists, ok := podGpusMap[key]
			if !ok {
				podGpusMap[key] = []string{id_device}
				continue
			}
			// Find insertion point while checking for duplicates
			insertIndex := 0
			for i, device := range devicelists {
				if device == id_device {
					// Skip if device already exists
					continue
				}
				if device > id_device {
					insertIndex = i
					break
				}
				insertIndex = i + 1
			}

			// Only insert if not a duplicate
			if insertIndex < len(devicelists) && devicelists[insertIndex] != id_device {
				devicelists = append(devicelists, "")
				copy(devicelists[insertIndex+1:], devicelists[insertIndex:])
				devicelists[insertIndex] = id_device
			} else if insertIndex == len(devicelists) {
				devicelists = append(devicelists, id_device)
			}
			podGpusMap[key] = devicelists
		}
	}

	// update podInfo gpu idx by podGpusMap
	for deviceID, podInfos := range deviceToPodsMap {
		for _, podInfo := range podInfos {

			key := fmt.Sprintf("%s-%s-%s", podInfo.Namespace, podInfo.Name, podInfo.Container)
			devicelists, ok := podGpusMap[key]
			if !ok {
				continue
			}

			for idx, id_device := range devicelists {
				if strings.HasSuffix(id_device, deviceID) {
					podInfo.GPU = strconv.Itoa(idx)
					break
				}
			}
		}
	}

	return deviceToPodsMap
}

func (p *PodMapper) toDeviceToPod(
	devicePods *podresourcesapi.ListPodResourcesResponse, deviceInfo deviceinfo.Provider,
) map[string]PodInfo {
	deviceToPodMap := make(map[string]PodInfo)

	for _, pod := range devicePods.GetPodResources() {
		for _, container := range pod.GetContainers() {
			for _, device := range container.GetDevices() {

				resourceName := device.GetResourceName()
				if resourceName != appconfig.NvidiaResourceName && !slices.Contains(p.Config.NvidiaResourceNames, resourceName) {
					// Mig resources appear differently than GPU resources
					if !strings.HasPrefix(resourceName, appconfig.NvidiaMigResourcePrefix) {
						continue
					}
				}

				podInfo := PodInfo{
					Name:      pod.GetName(),
					Namespace: pod.GetNamespace(),
					Container: container.GetName(),
				}

				for _, deviceID := range device.GetDeviceIds() {
					if strings.HasPrefix(deviceID, appconfig.MIG_UUID_PREFIX) {
						migDevice, err := nvmlprovider.Client().GetMIGDeviceInfoByID(deviceID)
						if err == nil {
							giIdentifier := deviceinfo.GetGPUInstanceIdentifier(deviceInfo, migDevice.ParentUUID,
								uint(migDevice.GPUInstanceID))
							deviceToPodMap[giIdentifier] = podInfo
						}
						gpuUUID := deviceID[len(appconfig.MIG_UUID_PREFIX):]
						deviceToPodMap[gpuUUID] = podInfo
					} else if gkeMigDeviceIDMatches := gkeMigDeviceIDRegex.FindStringSubmatch(deviceID); gkeMigDeviceIDMatches != nil {
						var gpuIndex string
						var gpuInstanceID string
						for groupIdx, group := range gkeMigDeviceIDMatches {
							switch groupIdx {
							case 1:
								gpuIndex = group
							case 2:
								gpuInstanceID = group
							}
						}
						giIdentifier := fmt.Sprintf("%s-%s", gpuIndex, gpuInstanceID)
						deviceToPodMap[giIdentifier] = podInfo
					} else if strings.Contains(deviceID, gkeVirtualGPUDeviceIDSeparator) {
						deviceToPodMap[strings.Split(deviceID, gkeVirtualGPUDeviceIDSeparator)[0]] = podInfo
					} else if strings.Contains(deviceID, "::") {
						gpuInstanceID := strings.Split(deviceID, "::")[0]
						deviceToPodMap[gpuInstanceID] = podInfo
					}
					// Default mapping between deviceID and pod information
					deviceToPodMap[deviceID] = podInfo
				}
			}
		}
	}
	gpuIndexMap := make(map[string]int)
	for i, gpu := range deviceInfo.GPUs() {
		gpuIndexMap[gpu.DeviceInfo.UUID] = i
	}
	// add the index of the GPU in the pod
	podGpusMap := make(map[string][]string)
	for deviceID, podInfo := range deviceToPodMap {
		gpuIndex, ok := gpuIndexMap[deviceID]

		if !ok {
			slog.Error("GPU not found in deviceInfo", "deviceID", deviceID, "gpuIndexMap", gpuIndexMap)
			continue
		}

		key := fmt.Sprintf("%s-%s-%s", podInfo.Namespace, podInfo.Name, podInfo.Container)
		id_device := fmt.Sprintf("%02d_%s", gpuIndex, deviceID)

		devicelists, ok := podGpusMap[key]
		if !ok {
			podGpusMap[key] = []string{id_device}
			continue
		}
		// Find insertion point while checking for duplicates
		insertIndex := 0
		for i, device := range devicelists {
			if device == id_device {
				// Skip if device already exists
				continue
			}
			if device > id_device {
				insertIndex = i
				break
			}
			insertIndex = i + 1
		}

		// Only insert if not a duplicate
		if insertIndex < len(devicelists) && devicelists[insertIndex] != id_device {
			devicelists = append(devicelists, "")
			copy(devicelists[insertIndex+1:], devicelists[insertIndex:])
			devicelists[insertIndex] = id_device
		} else if insertIndex == len(devicelists) {
			devicelists = append(devicelists, id_device)
		}
		podGpusMap[key] = devicelists
	}

	// update podInfo gpu idx by podGpusMap
	for deviceID, podInfo := range deviceToPodMap {
		key := fmt.Sprintf("%s-%s-%s", podInfo.Namespace, podInfo.Name, podInfo.Container)
		devicelists, ok := podGpusMap[key]
		if !ok {
			continue
		}

		for idx, id_device := range devicelists {
			if strings.HasSuffix(id_device, deviceID) {
				podInfo.GPU = strconv.Itoa(idx)
				break
			}
		}
	}

	return deviceToPodMap
}
