/*
Copyright 2018 The Kubernetes Authors.

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

package gce

import (
	"reflect"
	"sync"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"

	gce "google.golang.org/api/compute/v1"
	klog "k8s.io/klog/v2"
)

// MachineTypeKey is used to identify MachineType.
type MachineTypeKey struct {
	Zone        string
	MachineType string
}

type machinesCacheValue struct {
	machineType *gce.MachineType
	err         error
}

// GceCache is used for caching cluster resources state.
//
// It is needed to:
// - keep track of MIGs in the cluster,
// - keep track of MIGs to instances mapping,
// - keep track of MIGs configuration such as target size and basename,
// - keep track of resource limiters and machine types,
// - limit repetitive GCE API calls.
//
// Cache keeps these values and gives access to getters, setters and
// invalidators all guarded with mutex. Cache does not refresh the data by
// itself - it just provides an interface enabling access to this data.
type GceCache struct {
	cacheMutex sync.Mutex

	// Cache content.
	migs                      map[GceRef]Mig
	instancesToMig            map[GceRef]GceRef
	instancesFromUnknownMig   map[GceRef]bool
	resourceLimiter           *cloudprovider.ResourceLimiter
	autoscalingOptionsCache   map[GceRef]map[string]string
	machinesCache             map[MachineTypeKey]machinesCacheValue
	migTargetSizeCache        map[GceRef]int64
	migBaseNameCache          map[GceRef]string
	instanceTemplateNameCache map[GceRef]string
	instanceTemplatesCache    map[GceRef]*gce.InstanceTemplate
}

// NewGceCache creates empty GceCache.
func NewGceCache() *GceCache {
	return &GceCache{
		migs:                      map[GceRef]Mig{},
		instancesToMig:            map[GceRef]GceRef{},
		instancesFromUnknownMig:   map[GceRef]bool{},
		autoscalingOptionsCache:   map[GceRef]map[string]string{},
		machinesCache:             map[MachineTypeKey]machinesCacheValue{},
		migTargetSizeCache:        map[GceRef]int64{},
		migBaseNameCache:          map[GceRef]string{},
		instanceTemplateNameCache: map[GceRef]string{},
		instanceTemplatesCache:    map[GceRef]*gce.InstanceTemplate{},
	}
}

// RegisterMig returns true if the node group wasn't in cache before, or its config was updated.
func (gc *GceCache) RegisterMig(newMig Mig) bool {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	oldMig, found := gc.migs[newMig.GceRef()]
	if found {
		if !reflect.DeepEqual(oldMig, newMig) {
			gc.migs[newMig.GceRef()] = newMig
			klog.V(4).Infof("Updated Mig %s", newMig.GceRef().String())
			return true
		}
		return false
	}

	klog.V(1).Infof("Registering %s", newMig.GceRef().String())
	gc.migs[newMig.GceRef()] = newMig
	return true
}

// UnregisterMig returns true if the node group has been removed, and false if it was already missing from cache.
func (gc *GceCache) UnregisterMig(toBeRemoved Mig) bool {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	_, found := gc.migs[toBeRemoved.GceRef()]
	if found {
		klog.V(1).Infof("Unregistered Mig %s", toBeRemoved.GceRef().String())
		delete(gc.migs, toBeRemoved.GceRef())
		gc.removeMigInstances(toBeRemoved.GceRef())
		return true
	}
	return false
}

// GetMig returns a MIG for a given GceRef.
func (gc *GceCache) GetMig(migRef GceRef) (Mig, bool) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	mig, found := gc.migs[migRef]
	return mig, found
}

// GetMigs returns a copy of migs list.
func (gc *GceCache) GetMigs() []Mig {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	migs := make([]Mig, 0, len(gc.migs))
	for _, mig := range gc.migs {
		migs = append(migs, mig)
	}
	return migs
}

// GetMigForInstance returns the cached MIG for instance GceRef
func (gc *GceCache) GetMigForInstance(instanceRef GceRef) (GceRef, bool) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	migRef, found := gc.instancesToMig[instanceRef]
	if found {
		klog.V(5).Infof("MIG cache hit for %s", instanceRef)
	}
	return migRef, found
}

// IsMigUnknownForInstance checks if MIG was marked as unknown for instance, meaning that
// a Mig to which this instance should belong does not list it as one of its instances.
func (gc *GceCache) IsMigUnknownForInstance(instanceRef GceRef) bool {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	unknown, _ := gc.instancesFromUnknownMig[instanceRef]
	if unknown {
		klog.V(5).Infof("Unknown MIG cache hit for %s", instanceRef)
	}
	return unknown
}

// SetMigInstances sets instances for a given Mig ref
func (gc *GceCache) SetMigInstances(migRef GceRef, instances []cloudprovider.Instance) error {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.removeMigInstances(migRef)
	for _, instance := range instances {
		instanceRef, err := GceRefFromProviderId(instance.Id)
		if err != nil {
			return err
		}
		delete(gc.instancesFromUnknownMig, instanceRef)
		gc.instancesToMig[instanceRef] = migRef
	}
	return nil
}

// MarkInstanceMigUnknown sets instance MIG to unknown, meaning that a Mig to which
// this instance should belong does not list it as one of its instances.
func (gc *GceCache) MarkInstanceMigUnknown(instanceRef GceRef) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.instancesFromUnknownMig[instanceRef] = true
}

// InvalidateInstancesToMig clears the instance to mig mapping for a GceRef
func (gc *GceCache) InvalidateInstancesToMig(migRef GceRef) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	klog.V(5).Infof("Mig instances cache invalidated for %s", migRef)
	gc.removeMigInstances(migRef)
}

// InvalidateAllInstancesToMig clears the instance to mig cache
func (gc *GceCache) InvalidateAllInstancesToMig() {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	klog.V(5).Infof("Instances to migs cache invalidated")
	gc.instancesToMig = make(map[GceRef]GceRef)
	gc.instancesFromUnknownMig = make(map[GceRef]bool)
}

func (gc *GceCache) removeMigInstances(migRef GceRef) {
	for instanceRef, instanceMigRef := range gc.instancesToMig {
		if migRef == instanceMigRef {
			delete(gc.instancesToMig, instanceRef)
			delete(gc.instancesFromUnknownMig, instanceRef)
		}
	}
}

// SetAutoscalingOptions stores autoscaling options strings obtained from IT.
func (gc *GceCache) SetAutoscalingOptions(ref GceRef, options map[string]string) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()
	gc.autoscalingOptionsCache[ref] = options
}

// GetAutoscalingOptions return autoscaling options strings obtained from IT.
func (gc *GceCache) GetAutoscalingOptions(ref GceRef) map[string]string {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()
	return gc.autoscalingOptionsCache[ref]
}

// SetResourceLimiter sets resource limiter.
func (gc *GceCache) SetResourceLimiter(resourceLimiter *cloudprovider.ResourceLimiter) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.resourceLimiter = resourceLimiter
}

// GetResourceLimiter returns resource limiter.
func (gc *GceCache) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	return gc.resourceLimiter, nil
}

// GetMigTargetSize returns the cached targetSize for a GceRef
func (gc *GceCache) GetMigTargetSize(ref GceRef) (int64, bool) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	size, found := gc.migTargetSizeCache[ref]
	if found {
		klog.V(5).Infof("Target size cache hit for %s", ref)
	}
	return size, found
}

// SetMigTargetSize sets targetSize for a GceRef
func (gc *GceCache) SetMigTargetSize(ref GceRef, size int64) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.migTargetSizeCache[ref] = size
}

// InvalidateMigTargetSize clears the target size cache
func (gc *GceCache) InvalidateMigTargetSize(ref GceRef) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	if _, found := gc.migTargetSizeCache[ref]; found {
		klog.V(5).Infof("Target size cache invalidated for %s", ref)
		delete(gc.migTargetSizeCache, ref)
	}
}

// InvalidateAllMigTargetSizes clears the target size cache
func (gc *GceCache) InvalidateAllMigTargetSizes() {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	klog.V(5).Infof("Target size cache invalidated")
	gc.migTargetSizeCache = map[GceRef]int64{}
}

// GetMigInstanceTemplateName returns the cached instance template ref for a mig GceRef
func (gc *GceCache) GetMigInstanceTemplateName(ref GceRef) (string, bool) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	templateName, found := gc.instanceTemplateNameCache[ref]
	if found {
		klog.V(5).Infof("Instance template names cache hit for %s", ref)
	}
	return templateName, found
}

// SetMigInstanceTemplateName sets instance template ref for a mig GceRef
func (gc *GceCache) SetMigInstanceTemplateName(ref GceRef, templateName string) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.instanceTemplateNameCache[ref] = templateName
}

// InvalidateMigInstanceTemplateName clears the instance template ref cache for a mig GceRef
func (gc *GceCache) InvalidateMigInstanceTemplateName(ref GceRef) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	if _, found := gc.instanceTemplateNameCache[ref]; found {
		klog.V(5).Infof("Instance template names cache invalidated for %s", ref)
		delete(gc.instanceTemplateNameCache, ref)
	}
}

// InvalidateAllMigInstanceTemplateNames clears the instance template ref cache
func (gc *GceCache) InvalidateAllMigInstanceTemplateNames() {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	klog.V(5).Infof("Instance template names cache invalidated")
	gc.instanceTemplateNameCache = map[GceRef]string{}
}

// GetMigInstanceTemplate returns the cached gce.InstanceTemplate for a mig GceRef
func (gc *GceCache) GetMigInstanceTemplate(ref GceRef) (*gce.InstanceTemplate, bool) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	instanceTemplate, found := gc.instanceTemplatesCache[ref]
	if found {
		klog.V(5).Infof("Instance template cache hit for %s", ref)
	}
	return instanceTemplate, found
}

// SetMigInstanceTemplate sets gce.InstanceTemplate for a mig GceRef
func (gc *GceCache) SetMigInstanceTemplate(ref GceRef, instanceTemplate *gce.InstanceTemplate) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.instanceTemplatesCache[ref] = instanceTemplate
}

// InvalidateMigInstanceTemplate clears the instance template cache for a mig GceRef
func (gc *GceCache) InvalidateMigInstanceTemplate(ref GceRef) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	if _, found := gc.instanceTemplatesCache[ref]; found {
		klog.V(5).Infof("Instance template cache invalidated for %s", ref)
		delete(gc.instanceTemplatesCache, ref)
	}
}

// InvalidateAllMigInstanceTemplates clears the instance template cache
func (gc *GceCache) InvalidateAllMigInstanceTemplates() {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	klog.V(5).Infof("Instance template cache invalidated")
	gc.instanceTemplatesCache = map[GceRef]*gce.InstanceTemplate{}
}

// GetMachineFromCache retrieves machine type from cache under lock.
func (gc *GceCache) GetMachineFromCache(machineType string, zone string) (*gce.MachineType, error) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	cv, ok := gc.machinesCache[MachineTypeKey{zone, machineType}]
	if !ok {
		return nil, nil
	}
	if cv.err != nil {
		return nil, cv.err
	}
	return cv.machineType, nil
}

// AddMachineToCache adds machine to cache under lock.
func (gc *GceCache) AddMachineToCache(machineType string, zone string, machine *gce.MachineType) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.machinesCache[MachineTypeKey{zone, machineType}] = machinesCacheValue{machineType: machine}
}

// AddMachineToCacheWithError adds machine to cache under lock.
func (gc *GceCache) AddMachineToCacheWithError(machineType string, zone string, err error) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.machinesCache[MachineTypeKey{zone, machineType}] = machinesCacheValue{err: err}
}

// SetMachinesCache sets the machines cache under lock.
func (gc *GceCache) SetMachinesCache(machinesCache map[MachineTypeKey]*gce.MachineType) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()

	gc.machinesCache = map[MachineTypeKey]machinesCacheValue{}
	for k, v := range machinesCache {
		gc.machinesCache[k] = machinesCacheValue{machineType: v}
	}
}

// SetMigBasename sets basename for given mig in cache
func (gc *GceCache) SetMigBasename(migRef GceRef, basename string) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()
	gc.migBaseNameCache[migRef] = basename
}

// GetMigBasename get basename for given mig from cache.
func (gc *GceCache) GetMigBasename(migRef GceRef) (basename string, found bool) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()
	basename, found = gc.migBaseNameCache[migRef]
	return
}

// InvalidateMigBasename invalidates basename entry for given mig.
func (gc *GceCache) InvalidateMigBasename(migRef GceRef) {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()
	delete(gc.migBaseNameCache, migRef)
}

// InvalidateAllMigBasenames invalidates all basename entries.
func (gc *GceCache) InvalidateAllMigBasenames() {
	gc.cacheMutex.Lock()
	defer gc.cacheMutex.Unlock()
	gc.migBaseNameCache = make(map[GceRef]string)
}
