"""Authored synthetic corpus: independent Lee skeleton and ITK boundary EDT.

No reference implementation is copied. Requires numpy 2.3.5, scikit-image
0.26.0 and SimpleITK 2.5.3. All grids have a background halo. The two filters
are qualified separately; this is not an ITK MedialThickness filter claim.
"""
import gzip
import hashlib
import json
from pathlib import Path

import numpy as np
import skimage
from skimage.morphology import skeletonize
import SimpleITK as sitk

assert np.__version__ == '2.3.5' and skimage.__version__ == '0.26.0'
assert sitk.Version_VersionString() == '2.5.3'
cases = []
for spacing in ((1., 1., 1.), (.5, .5, .5), (.5, .8, 1.2)):
    n = 25
    z, y, x = np.mgrid[:n, :n, :n]
    x, y, z = (x-n//2)*spacing[0], (y-n//2)*spacing[1], (z-n//2)*spacing[2]
    shapes = {
        'tube': (x*x+y*y <= 9) & (abs(z) <= 4),
        'sphere': x*x+y*y+z*z <= 9,
        'slab': (abs(z) <= 1.2) & (abs(x) <= 4) & (abs(y) <= 4),
        'branch': ((x*x+y*y <= 2.25) & (abs(z) <= 4)) |
                  (((x-z/2)**2+y*y <= 2.25) & (z >= 0) & (z <= 4)),
        'cavity': (x*x+y*y+z*z <= 16) & (x*x+y*y+z*z >= 4),
    }
    for name, mask in shapes.items():
        a = mask.astype(np.uint8)
        assert not any(np.any(a.take(i, axis=k)) for k in range(3) for i in (0, n-1))
        skeleton = skeletonize(a.astype(bool), method='lee')
        image = sitk.GetImageFromArray(a)
        image.SetSpacing(spacing)
        distance = sitk.GetArrayFromImage(sitk.SignedMaurerDistanceMap(
            image, insideIsPositive=True, squaredDistance=False, useImageSpacing=True))
        # Store all foreground distances, independently of either skeleton.
        fg = np.flatnonzero(a)
        cases.append({'name': name, 'dimensions': [n,n,n], 'spacing': spacing,
                      'foreground': fg.tolist(),
                      'skeleton': np.flatnonzero(skeleton).tolist(),
                      'distance': distance.ravel()[fg].astype(float).tolist()})
receipt = {'numpy': np.__version__, 'skimage': skimage.__version__,
           'simpleitk': sitk.Version_VersionString(), 'itk': sitk.Version_ITKVersionString(),
           'generator_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
           'cases': cases}
payload = json.dumps(receipt, separators=(',', ':'), allow_nan=False).encode()
out = Path(__file__).with_name('oracle.json.gz')
with out.open('wb') as f:
    with gzip.GzipFile(fileobj=f, mode='wb', mtime=0) as g:
        g.write(payload)
print(f'{len(cases)} cases; {len(payload)} decoded bytes; {out.stat().st_size} stored bytes')
