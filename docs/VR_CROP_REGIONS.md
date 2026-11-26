# VR crop regions

`VRClip.SetCropRegionFlags` selects normalized crop regions while retaining the
six crop planes. `VRCropSubvolume` keeps the central box, `VRCropFence` keeps
samples inside at least one axis interval, and `VRCropCross` keeps samples inside
at least two. Inverted variants retain their complements. Boundaries belong to
the inside interval. Bits use `x + 3*y + 9*z` over below/inside/above intervals;
bit 26 remains exact. These masks follow the [VTK volume mapper convention](https://github.com/Kitware/VTK/blob/master/Rendering/Volume/vtkVolumeMapper.h).

Zero flags retain the legacy central box. Custom nonzero masks must fit 27 bits.
View state V25 carries `NormalizedCropBox.RegionFlags`; earlier contract versions
refuse nonzero masks. Disabled crop cannot carry a mask. Region unions retain
outer ray intervals and reject samples individually rather than shortening the
ray to the central box. Independent 27-cell tests cover named masks, boundaries,
version validation and disjoint retained intervals.

