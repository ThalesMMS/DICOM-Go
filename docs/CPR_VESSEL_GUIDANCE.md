# Vessel-guided CPR path analysis

Set `CPRPathAssistantOptions.VesselGuided` to prepare a modality-value vesselness
response around 2-5 patient-space waypoints and use it as fast-marching
`SpeedVolume`. Six logarithmic scales span 0.75-4 mm. The Frangi shape response
uses no structureness normalization, so bright calcified wall values do not
set the contrast-lumen intensity scale. An explicit speed volume or Dijkstra
cannot be combined with this option.

The ROI includes the requested search margin plus 16 mm filter context. Its
analysis grid retains the source's orientation and patient-space endpoints;
fine acquisitions use spacing no greater than 0.75 mm, while coarser axes retain
their acquired resolution. Admission limits the derived region to 8 million
voxels and vesselness slab working buffers to 64 MiB. Source-store memory is
separate. Oversized regions return a typed `vessel_voxels` resource limit; reduce
the search margin or analyse a shorter vessel section. The search
margin must include the vessel's bends as well as its endpoints.

Sampling, filtering and fast marching honor cancellation. The temporary speed
volume owns its store and closes after the path is produced. Source values and
geometry remain unchanged. This operation computes geometry, not an image;
application CPR reconstruction continues through its GPU worker.

`TestVesselGuidedCPRStaysInBifurcatingCalcifiedLumen` checks both branches of a
signed modality phantom with a 350 HU lumen and an asymmetric 1800 HU calcified
wall against its independent analytic lumen. Other tests cover active
cancellation, conflicting options, memory admission and fine oblique geometry.
These are synthetic qualification results, not a clinical study.
