// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Handler for "raw" containers.
package raw

import (
	"fmt"
	"io/ioutil"
	"path"
	"strconv"
	"strings"

	"code.google.com/p/go.exp/inotify"
	dockerlibcontainer "github.com/docker/libcontainer"
	"github.com/docker/libcontainer/cgroups"
	cgroup_fs "github.com/docker/libcontainer/cgroups/fs"
	"github.com/docker/libcontainer/network"
	"github.com/golang/glog"
	"github.com/google/cadvisor/container"
	"github.com/google/cadvisor/container/libcontainer"
	"github.com/google/cadvisor/fs"
	"github.com/google/cadvisor/info"
	"github.com/google/cadvisor/utils"
	"github.com/google/cadvisor/utils/sysinfo"
)

type rawContainerHandler struct {
	// Name of the container for this handler.
	name               string
	cgroup             *cgroups.Cgroup
	cgroupSubsystems   *libcontainer.CgroupSubsystems
	machineInfoFactory info.MachineInfoFactory

	// Inotify event watcher.
	watcher *inotify.Watcher

	// Signal for watcher thread to stop.
	stopWatcher chan error

	// Containers being watched for new subcontainers.
	watches map[string]struct{}

	// Cgroup paths being watchd for new subcontainers
	cgroupWatches map[string]struct{}

	// Absolute path to the cgroup hierarchies of this container.
	// (e.g.: "cpu" -> "/sys/fs/cgroup/cpu/test")
	cgroupPaths map[string]string

	// Equivalent libcontainer state for this container.
	libcontainerState dockerlibcontainer.State

	// Whether this container has network isolation enabled.
	hasNetwork bool

	fsInfo         fs.FsInfo
	externalMounts []mount
}

func newRawContainerHandler(name string, cgroupSubsystems *libcontainer.CgroupSubsystems, machineInfoFactory info.MachineInfoFactory) (container.ContainerHandler, error) {
	// Create the cgroup paths.
	cgroupPaths := make(map[string]string, len(cgroupSubsystems.MountPoints))
	for key, val := range cgroupSubsystems.MountPoints {
		cgroupPaths[key] = path.Join(val, name)
	}

	// TODO(vmarmol): Get from factory.
	fsInfo, err := fs.NewFsInfo()
	if err != nil {
		return nil, err
	}
	cHints, err := getContainerHintsFromFile(*argContainerHints)
	if err != nil {
		return nil, err
	}

	// Generate the equivalent libcontainer state for this container.
	libcontainerState := dockerlibcontainer.State{
		CgroupPaths: cgroupPaths,
	}

	hasNetwork := false
	var externalMounts []mount
	for _, container := range cHints.AllHosts {
		if name == container.FullName {
			libcontainerState.NetworkState = network.NetworkState{
				VethHost:  container.NetworkInterface.VethHost,
				VethChild: container.NetworkInterface.VethChild,
			}
			hasNetwork = true
			externalMounts = container.Mounts
			break
		}
	}

	return &rawContainerHandler{
		name: name,
		cgroup: &cgroups.Cgroup{
			Parent: "/",
			Name:   name,
		},
		cgroupSubsystems:   cgroupSubsystems,
		machineInfoFactory: machineInfoFactory,
		stopWatcher:        make(chan error),
		watches:            make(map[string]struct{}),
		cgroupWatches:      make(map[string]struct{}),
		cgroupPaths:        cgroupPaths,
		libcontainerState:  libcontainerState,
		fsInfo:             fsInfo,
		hasNetwork:         hasNetwork,
		externalMounts:     externalMounts,
	}, nil
}

func (self *rawContainerHandler) ContainerReference() (info.ContainerReference, error) {
	// We only know the container by its one name.
	return info.ContainerReference{
		Name: self.name,
	}, nil
}

func readString(dirpath string, file string) string {
	cgroupFile := path.Join(dirpath, file)

	// Ignore non-existent files
	if !utils.FileExists(cgroupFile) {
		return ""
	}

	// Read
	out, err := ioutil.ReadFile(cgroupFile)
	if err != nil {
		glog.Errorf("raw driver: Failed to read %q: %s", cgroupFile, err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readInt64(dirpath string, file string) uint64 {
	out := readString(dirpath, file)
	if out == "" {
		return 0
	}

	val, err := strconv.ParseUint(out, 10, 64)
	if err != nil {
		glog.Errorf("raw driver: Failed to parse int %q from file %q: %s", out, path.Join(dirpath, file), err)
		return 0
	}

	return val
}

func (self *rawContainerHandler) GetRootNetworkDevices() ([]info.NetInfo, error) {
	nd := []info.NetInfo{}
	if self.name == "/" {
		mi, err := self.machineInfoFactory.GetMachineInfo()
		if err != nil {
			return nd, err
		}
		return mi.NetworkDevices, nil
	}
	return nd, nil
}

func (self *rawContainerHandler) GetSpec() (info.ContainerSpec, error) {
	var spec info.ContainerSpec

	// The raw driver assumes unified hierarchy containers.

	// Get machine info.
	mi, err := self.machineInfoFactory.GetMachineInfo()
	if err != nil {
		return spec, err
	}

	// CPU.
	cpuRoot, ok := self.cgroupPaths["cpu"]
	if ok {
		if utils.FileExists(cpuRoot) {
			spec.HasCpu = true
			spec.Cpu.Limit = readInt64(cpuRoot, "cpu.shares")
		}
	}

	// Cpu Mask.
	// This will fail for non-unified hierarchies. We'll return the whole machine mask in that case.
	cpusetRoot, ok := self.cgroupPaths["cpuset"]
	if ok {
		if utils.FileExists(cpusetRoot) {
			spec.HasCpu = true
			spec.Cpu.Mask = readString(cpusetRoot, "cpuset.cpus")
			if spec.Cpu.Mask == "" {
				spec.Cpu.Mask = fmt.Sprintf("0-%d", mi.NumCores-1)
			}
		}
	}

	// Memory.
	memoryRoot, ok := self.cgroupPaths["memory"]
	if ok {
		if utils.FileExists(memoryRoot) {
			spec.HasMemory = true
			spec.Memory.Limit = readInt64(memoryRoot, "memory.limit_in_bytes")
			spec.Memory.SwapLimit = readInt64(memoryRoot, "memory.memsw.limit_in_bytes")
		}
	}

	// Fs.
	if self.name == "/" || self.externalMounts != nil {
		spec.HasFilesystem = true
	}

	//Network
	spec.HasNetwork = self.hasNetwork

	// DiskIo.
	if blkioRoot, ok := self.cgroupPaths["blkio"]; ok && utils.FileExists(blkioRoot) {
		spec.HasDiskIo = true
	}

	// Check physical network devices for root container.
	nd, err := self.GetRootNetworkDevices()
	if err != nil {
		return spec, err
	}
	if len(nd) != 0 {
		spec.HasNetwork = true
	}
	return spec, nil
}

func (self *rawContainerHandler) getFsStats(stats *info.ContainerStats) error {
	// Get Filesystem information only for the root cgroup.
	if self.name == "/" {
		filesystems, err := self.fsInfo.GetGlobalFsInfo()
		if err != nil {
			return err
		}
		for _, fs := range filesystems {
			stats.Filesystem = append(stats.Filesystem,
				info.FsStats{
					Device:          fs.Device,
					Limit:           fs.Capacity,
					Usage:           fs.Capacity - fs.Free,
					ReadsCompleted:  fs.DiskStats.ReadsCompleted,
					ReadsMerged:     fs.DiskStats.ReadsMerged,
					SectorsRead:     fs.DiskStats.SectorsRead,
					ReadTime:        fs.DiskStats.ReadTime,
					WritesCompleted: fs.DiskStats.WritesCompleted,
					WritesMerged:    fs.DiskStats.WritesMerged,
					SectorsWritten:  fs.DiskStats.SectorsWritten,
					WriteTime:       fs.DiskStats.WriteTime,
					IoInProgress:    fs.DiskStats.IoInProgress,
					IoTime:          fs.DiskStats.IoTime,
					WeightedIoTime:  fs.DiskStats.WeightedIoTime,
				})
		}
	} else if len(self.externalMounts) > 0 {
		var mountSet map[string]struct{}
		mountSet = make(map[string]struct{})
		for _, mount := range self.externalMounts {
			mountSet[mount.HostDir] = struct{}{}
		}
		filesystems, err := self.fsInfo.GetFsInfoForPath(mountSet)
		if err != nil {
			return err
		}
		for _, fs := range filesystems {
			stats.Filesystem = append(stats.Filesystem,
				info.FsStats{
					Device:          fs.Device,
					Limit:           fs.Capacity,
					Usage:           fs.Capacity - fs.Free,
					ReadsCompleted:  fs.DiskStats.ReadsCompleted,
					ReadsMerged:     fs.DiskStats.ReadsMerged,
					SectorsRead:     fs.DiskStats.SectorsRead,
					ReadTime:        fs.DiskStats.ReadTime,
					WritesCompleted: fs.DiskStats.WritesCompleted,
					WritesMerged:    fs.DiskStats.WritesMerged,
					SectorsWritten:  fs.DiskStats.SectorsWritten,
					WriteTime:       fs.DiskStats.WriteTime,
					IoInProgress:    fs.DiskStats.IoInProgress,
					IoTime:          fs.DiskStats.IoTime,
					WeightedIoTime:  fs.DiskStats.WeightedIoTime,
				})
		}
	}
	return nil
}

func (self *rawContainerHandler) GetStats() (*info.ContainerStats, error) {
	stats, err := libcontainer.GetStats(self.cgroupPaths, &self.libcontainerState)
	if err != nil {
		return stats, err
	}

	err = self.getFsStats(stats)
	if err != nil {
		return stats, err
	}

	// Fill in network stats for root.
	nd, err := self.GetRootNetworkDevices()
	if err != nil {
		return stats, err
	}
	if len(nd) != 0 {
		// ContainerStats only reports stat for one network device.
		// TODO(rjnagal): Handle multiple physical network devices.
		stats.Network, err = sysinfo.GetNetworkStats(nd[0].Name)
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (self *rawContainerHandler) GetCgroupPath(resource string) (string, error) {
	path, ok := self.cgroupPaths[resource]
	if !ok {
		return "", fmt.Errorf("could not find path for resource %q for container %q\n", resource, self.name)
	}
	return path, nil
}

// Lists all directories under "path" and outputs the results as children of "parent".
func listDirectories(dirpath string, parent string, recursive bool, output map[string]struct{}) error {
	// Ignore if this hierarchy does not exist.
	if !utils.FileExists(dirpath) {
		return nil
	}

	entries, err := ioutil.ReadDir(dirpath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// We only grab directories.
		if entry.IsDir() {
			name := path.Join(parent, entry.Name())
			output[name] = struct{}{}

			// List subcontainers if asked to.
			if recursive {
				err := listDirectories(path.Join(dirpath, entry.Name()), name, true, output)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (self *rawContainerHandler) ListContainers(listType container.ListType) ([]info.ContainerReference, error) {
	containers := make(map[string]struct{})
	for _, cgroupPath := range self.cgroupPaths {
		err := listDirectories(cgroupPath, self.name, listType == container.ListRecursive, containers)
		if err != nil {
			return nil, err
		}
	}

	// Make into container references.
	ret := make([]info.ContainerReference, 0, len(containers))
	for cont := range containers {
		ret = append(ret, info.ContainerReference{
			Name: cont,
		})
	}

	return ret, nil
}

func (self *rawContainerHandler) ListThreads(listType container.ListType) ([]int, error) {
	// TODO(vmarmol): Implement
	return nil, nil
}

func (self *rawContainerHandler) ListProcesses(listType container.ListType) ([]int, error) {
	return cgroup_fs.GetPids(self.cgroup)
}

func (self *rawContainerHandler) watchDirectory(dir string, containerName string) error {
	err := self.watcher.AddWatch(dir, inotify.IN_CREATE|inotify.IN_DELETE|inotify.IN_MOVE)
	if err != nil {
		return err
	}
	self.watches[containerName] = struct{}{}
	self.cgroupWatches[dir] = struct{}{}

	// TODO(vmarmol): We should re-do this once we're done to ensure directories were not added in the meantime.
	// Watch subdirectories as well.
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			err = self.watchDirectory(path.Join(dir, entry.Name()), path.Join(containerName, entry.Name()))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (self *rawContainerHandler) processEvent(event *inotify.Event, events chan container.SubcontainerEvent) error {
	// Convert the inotify event type to a container create or delete.
	var eventType container.SubcontainerEventType
	switch {
	case (event.Mask & inotify.IN_CREATE) > 0:
		eventType = container.SubcontainerAdd
	case (event.Mask & inotify.IN_DELETE) > 0:
		eventType = container.SubcontainerDelete
	case (event.Mask & inotify.IN_MOVED_FROM) > 0:
		eventType = container.SubcontainerDelete
	case (event.Mask & inotify.IN_MOVED_TO) > 0:
		eventType = container.SubcontainerAdd
	default:
		// Ignore other events.
		return nil
	}

	// Derive the container name from the path name.
	var containerName string
	for _, mount := range self.cgroupSubsystems.Mounts {
		mountLocation := path.Clean(mount.Mountpoint) + "/"
		if strings.HasPrefix(event.Name, mountLocation) {
			containerName = event.Name[len(mountLocation)-1:]
			break
		}
	}
	if containerName == "" {
		return fmt.Errorf("unable to detect container from watch event on directory %q", event.Name)
	}

	// Maintain the watch for the new or deleted container.
	switch {
	case eventType == container.SubcontainerAdd:
		_, alreadyWatched := self.watches[containerName]

		// New container was created, watch it.
		err := self.watchDirectory(event.Name, containerName)
		if err != nil {
			return err
		}

		// Only report container creation once.
		if alreadyWatched {
			return nil
		}
	case eventType == container.SubcontainerDelete:
		// Container was deleted, stop watching for it. Only delete the event if we registered it.
		if _, ok := self.cgroupWatches[event.Name]; ok {
			err := self.watcher.RemoveWatch(event.Name)
			if err != nil {
				return err
			}
			delete(self.cgroupWatches, event.Name)
		}

		// Only report container deletion once.
		if _, ok := self.watches[containerName]; !ok {
			return nil
		}
		delete(self.watches, containerName)
	default:
		return fmt.Errorf("unknown event type %v", eventType)
	}

	// Deliver the event.
	events <- container.SubcontainerEvent{
		EventType: eventType,
		Name:      containerName,
	}

	return nil
}

func (self *rawContainerHandler) WatchSubcontainers(events chan container.SubcontainerEvent) error {
	// Lazily initialize the watcher so we don't use it when not asked to.
	if self.watcher == nil {
		w, err := inotify.NewWatcher()
		if err != nil {
			return err
		}
		self.watcher = w
	}

	// Watch this container (all its cgroups) and all subdirectories.
	for _, cgroupPath := range self.cgroupPaths {
		err := self.watchDirectory(cgroupPath, self.name)
		if err != nil {
			return err
		}
	}

	// Process the events received from the kernel.
	go func() {
		for {
			select {
			case event := <-self.watcher.Event:
				err := self.processEvent(event, events)
				if err != nil {
					glog.Warningf("Error while processing event (%+v): %v", event, err)
				}
			case err := <-self.watcher.Error:
				glog.Warningf("Error while watching %q:", self.name, err)
			case <-self.stopWatcher:
				err := self.watcher.Close()
				if err == nil {
					self.stopWatcher <- err
					self.watcher = nil
					return
				}
			}
		}
	}()

	return nil
}

func (self *rawContainerHandler) StopWatchingSubcontainers() error {
	if self.watcher == nil {
		return fmt.Errorf("can't stop watch that has not started for container %q", self.name)
	}

	// Rendezvous with the watcher thread.
	self.stopWatcher <- nil
	return <-self.stopWatcher
}

func (self *rawContainerHandler) Exists() bool {
	// If any cgroup exists, the container is still alive.
	for _, cgroupPath := range self.cgroupPaths {
		if utils.FileExists(cgroupPath) {
			return true
		}
	}
	return false
}
