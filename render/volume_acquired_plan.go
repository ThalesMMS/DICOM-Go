package render

// AcquiredResidencyKey inspects existing decode identity without triggering
// decoding or canonical regularization. Zero means no live source is prepared.
func (v *Volume) AcquiredResidencyKey() VolumeResidencyKey {
	if v == nil || v.closed.Load() {
		return VolumeResidencyKey{}
	}
	v.snapshotMu.Lock()
	defer v.snapshotMu.Unlock()
	if v.store == nil || v.generation == 0 {
		return VolumeResidencyKey{}
	}
	return VolumeResidencyKey{StoreIdentity: v.store.identity, Generation: v.generation}
}

// PlanAcquiredRegularization returns metadata without decoding or preparing
// either an acquired tensor or a canonical CPU regularized volume.
func (v *Volume) PlanAcquiredRegularization() (VolumeRegularizationPlan, error) {
	if v == nil || v.closed.Load() {
		return VolumeRegularizationPlan{}, ErrVolumeStoreClosed
	}
	return PlanVolumeRegularization(v.geometry)
}
