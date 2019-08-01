/*
Copyright 2018 The Ceph-CSI Authors.

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

package rbd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ceph/ceph-csi/pkg/util"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	rbdTonbd  = "rbd-nbd"
	moduleNbd = "nbd"

	accessTypeKRbd = "krbd"
	accessTypeNbd  = "nbd"

	rbd = "rbd"
)

var hasNBD = false

func init() {
	hasNBD = checkRbdNbdTools()
}

// rbdDeviceInfo strongly typed JSON spec for rbd device list output (of type krbd)
type rbdDeviceInfo struct {
	ID     string `json:"id"`
	Pool   string `json:"pool"`
	Name   string `json:"name"`
	Device string `json:"device"`
}

// nbdDeviceInfo strongly typed JSON spec for rbd-nbd device list output (of type nbd)
// NOTE: There is a bug in rbd output that returns id as number for nbd, and string for krbd, thus
// requiring 2 different JSON structures to unmarshal the output.
// NOTE: image key is "name" in krbd output and "image" in nbd output, which is another difference
type nbdDeviceInfo struct {
	ID     int64  `json:"id"`
	Pool   string `json:"pool"`
	Name   string `json:"image"`
	Device string `json:"device"`
}

// rbdGetDeviceList queries rbd about mapped devices and returns a list of rbdDeviceInfo
// It will selectively list devices mapped using krbd or nbd as specified by accessType
func rbdGetDeviceList(accessType string) ([]rbdDeviceInfo, error) {
	// rbd device list --format json --device-type [krbd|nbd]
	var (
		rbdDeviceList []rbdDeviceInfo
		nbdDeviceList []nbdDeviceInfo
	)

	stdout, _, err := util.ExecCommand(rbd, "device", "list", "--format="+"json", "--device-type", accessType)
	if err != nil {
		return nil, fmt.Errorf("error getting device list from rbd for devices of type (%s): (%v)", accessType, err)
	}

	if accessType == accessTypeKRbd {
		err = json.Unmarshal(stdout, &rbdDeviceList)
	} else {
		err = json.Unmarshal(stdout, &nbdDeviceList)
	}
	if err != nil {
		return nil, fmt.Errorf("error to parse JSON output of device list for devices of type (%s): (%v)", accessType, err)
	}

	// convert output to a rbdDeviceInfo list for consumers
	if accessType == accessTypeNbd {
		for _, device := range nbdDeviceList {
			rbdDeviceList = append(
				rbdDeviceList,
				rbdDeviceInfo{
					ID:     strconv.FormatInt(device.ID, 10),
					Pool:   device.Pool,
					Name:   device.Name,
					Device: device.Device,
				})
		}
	}

	return rbdDeviceList, nil
}

// findDeviceMappingImage finds a devicePath, if available, based on image spec (pool/image) on the node.
func findDeviceMappingImage(pool, image string, useNbdDriver bool) (string, bool) {
	accessType := accessTypeKRbd
	if useNbdDriver {
		accessType = accessTypeNbd
	}

	rbdDeviceList, err := rbdGetDeviceList(accessType)
	if err != nil {
		klog.Warningf("failed to determine if image (%s/%s) is mapped to a device (%v)", pool, image, err)
		return "", false
	}

	for _, device := range rbdDeviceList {
		if device.Name == image && device.Pool == pool {
			return device.Device, true
		}
	}

	return "", false
}

// Stat a path, if it doesn't exist, retry maxRetries times.
func waitForPath(pool, image string, maxRetries int, useNbdDriver bool) (string, bool) {
	for i := 0; i < maxRetries; i++ {
		if i != 0 {
			time.Sleep(time.Second)
		}

		device, found := findDeviceMappingImage(pool, image, useNbdDriver)
		if found {
			return device, found
		}
	}

	return "", false
}

// Check if rbd-nbd tools are installed.
func checkRbdNbdTools() bool {
	_, err := execCommand("modprobe", []string{moduleNbd})
	if err != nil {
		klog.V(3).Infof("rbd-nbd: nbd modprobe failed with error %v", err)
		return false
	}
	if _, err := execCommand(rbdTonbd, []string{"--version"}); err != nil {
		klog.V(3).Infof("rbd-nbd: running rbd-nbd --version failed with error %v", err)
		return false
	}
	klog.V(3).Infof("rbd-nbd tools were found.")
	return true
}

func attachRBDImage(volOptions *rbdVolume, cr *util.Credentials) (string, error) {
	var err error

	image := volOptions.RbdImageName
	useNBD := false
	if volOptions.Mounter == rbdTonbd && hasNBD {
		useNBD = true
	}

	devicePath, found := waitForPath(volOptions.Pool, image, 1, useNBD)
	if !found {
		backoff := wait.Backoff{
			Duration: rbdImageWatcherInitDelay,
			Factor:   rbdImageWatcherFactor,
			Steps:    rbdImageWatcherSteps,
		}

		err = waitForrbdImage(backoff, volOptions, cr)

		if err != nil {
			return "", err
		}
		devicePath, err = createPath(volOptions, cr)
	}

	return devicePath, err
}

func createPath(volOpt *rbdVolume, cr *util.Credentials) (string, error) {
	image := volOpt.RbdImageName
	imagePath := fmt.Sprintf("%s/%s", volOpt.Pool, image)

	klog.V(5).Infof("rbd: map mon %s", volOpt.Monitors)

	// Map options
	mapOptions := []string{
		"--id", cr.ID,
		"-m", volOpt.Monitors,
		"--keyfile=" + cr.KeyFile}

	// Construct map and unmap variants for the command
	mapOptions = append(mapOptions, "map", imagePath)
	unmapOptions := []string{"unmap", imagePath}

	// Choose access protocol
	useNBD := false
	accessType := accessTypeKRbd
	if volOpt.Mounter == rbdTonbd && hasNBD {
		accessType = accessTypeNbd
		useNBD = true
	}

	// Update options with device type selection
	mapOptions = append(mapOptions, "--device-type", accessType)
	unmapOptions = append(unmapOptions, "--device-type", accessType)

	// Execute map
	output, err := execCommand(rbd, mapOptions)
	if err != nil {
		klog.Warningf("rbd: map error %v, rbd output: %s", err, string(output))
		return "", fmt.Errorf("rbd: map failed %v, rbd output: %s", err, string(output))
	}
	devicePath := strings.TrimSuffix(string(output), "\n")

	return devicePath, nil
}

func waitForrbdImage(backoff wait.Backoff, volOptions *rbdVolume, cr *util.Credentials) error {
	image := volOptions.RbdImageName
	imagePath := fmt.Sprintf("%s/%s", volOptions.Pool, image)

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		used, rbdOutput, err := rbdStatus(volOptions, cr)
		if err != nil {
			return false, fmt.Errorf("fail to check rbd image status with: (%v), rbd output: (%s)", err, rbdOutput)
		}
		if (volOptions.DisableInUseChecks) && (used) {
			klog.V(2).Info("valid multi-node attach requested, ignoring watcher in-use result")
			return used, nil
		}
		return !used, nil
	})
	// return error if rbd image has not become available for the specified timeout
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("rbd image %s is still being used", imagePath)
	}
	// return error if any other errors were encountered during waiting for the image to become available
	return err
}

func detachRBDDevice(devicePath string) error {
	var err error
	var output []byte

	klog.V(3).Infof("rbd: unmap device %s", devicePath)

	accessType := accessTypeKRbd
	if strings.HasPrefix(devicePath, "/dev/nbd") {
		accessType = accessTypeNbd
	}
	options := []string{"unmap", "--device-type", accessType, devicePath}

	output, err = execCommand(rbd, options)
	if err != nil {
		return fmt.Errorf("rbd: unmap failed %v, rbd output: %s", err, string(output))
	}

	return nil
}
