#!/usr/bin/env python3
"""Own synthetic fixtures via the public SimpleITK API; no ITK source access."""
import argparse
import json
import math
from pathlib import Path

import SimpleITK as sitk


def rotation(angle):
    c, s = math.cos(angle), math.sin(angle)
    return [[c, -s, 0.0], [s, c, 0.0], [0.0, 0.0, 1.0]]


IDENTITY = rotation(0.0)


def geometry(size, origin=(0.0, 0.0, 0.0), spacing=(1.0, 1.0, 1.0), direction=IDENTITY):
    return dict(size=size, origin=origin, spacing=spacing, direction=direction)


def point(g, q):
    return [g['origin'][i] + sum(g['direction'][i][j] * g['spacing'][j] * q[j]
                                for j in range(3)) for i in range(3)]


def build_image(g, pattern):
    image = sitk.Image(g['size'], sitk.sitkFloat32)
    image.SetOrigin(g['origin'])
    image.SetSpacing(g['spacing'])
    image.SetDirection([v for row in g['direction'] for v in row])
    data = []
    for z in range(g['size'][2]):
        for y in range(g['size'][1]):
            for x in range(g['size'][0]):
                image[x, y, z] = pattern(x, y, z, point(g, [x, y, z]))
                data.append(image[x, y, z])  # Record the actual float32 input.
    return image, data


def definitions():
    yield 'identity', geometry([7, 6, 5]), lambda x, y, z, p: x*x + 3*y - 2*z + .25, geometry([7, 6, 5]), IDENTITY, [0., 0., 0.]
    for axis in range(3):
        size = [1, 1, 1]
        size[axis] = 43
        origin = [8., 8., 8.]
        origin[axis] = 2.75
        spacing = [1., 1., 1.]
        spacing[axis] = .25
        yield 'impulse_' + 'xyz'[axis], geometry([17, 17, 17]), lambda x, y, z, p: float(x == y == z == 8), geometry(size, origin, spacing), IDENTITY, [0., 0., 0.]
    yield 'constant_phases', geometry([11, 7, 5]), lambda x, y, z, p: 1., geometry([3, 3, 3], [5.25, 3., 2.], [.25, .25, .25]), IDENTITY, [0., 0., 0.]
    yield 'singleton', geometry([1, 1, 1]), lambda x, y, z, p: 7., geometry([3, 3, 3], [-.25, -.25, -.25], [.25, .25, .25]), IDENTITY, [0., 0., 0.]
    yield 'step_ringing', geometry([17, 5, 3]), lambda x, y, z, p: 100. if x >= 8 else 0., geometry([17, 1, 1], [6., 2., 1.], [.25, 1., 1.]), IDENTITY, [0., 0., 0.]
    source = geometry([15, 13, 11], [-5., 2., -9.], [1.4, .9, 2.2], rotation(.2))
    target = geometry([5, 4, 3], point(source, [3.25, 3.5, 3.75]), [1.1, .7, 1.7], rotation(-.1))
    affine = [[1.01, .025, -.01], [-.015, .98, .02], [.01, -.005, 1.02]]
    yield 'affine_oblique_anisotropic', source, lambda x, y, z, p: 120*math.sin(.3*p[0]) + 70*math.cos(.25*p[1])*math.sin(.2*p[2]) + 2*p[2], target, affine, [.2, -.1, .3]
    for i, q in enumerate([-.5000000001, -.5, -.4999999999, -.25, 0., .25, 9.75, 10.25, 10.4999999999, 10.5]):
        yield 'edge_' + str(i), geometry([11, 3, 3]), lambda x, y, z, p: x*x + 2*x + 3., geometry([1, 1, 1], [q, 1., 1.]), IDENTITY, [0., 0., 0.]
    yield 'corner_extension', geometry([7, 6, 5]), lambda x, y, z, p: x*x + y + 2*z, geometry([1, 1, 1], [-.25, -.25, -.25]), IDENTITY, [0., 0., 0.]


def generate():
    version, itk_version = sitk.Version_VersionString(), sitk.Version_ITKVersionString()
    if version != '2.5.3' or not itk_version.startswith('5.4'):
        raise RuntimeError('expected SimpleITK2.5.3 / ITK5.4, got ' + version + ' / ' + itk_version)
    cases = []
    for name, source, pattern, target, matrix, offset in definitions():
        image, data = build_image(source, pattern)
        transform = sitk.AffineTransform(3)
        transform.SetMatrix([v for row in matrix for v in row])
        transform.SetTranslation(offset)
        for window, interpolator in [('Lanczos', sitk.sitkLanczosWindowedSinc), ('Welch', sitk.sitkWelchWindowedSinc)]:
            filt = sitk.ResampleImageFilter()
            filt.SetSize(target['size'])
            filt.SetOutputOrigin(target['origin'])
            filt.SetOutputSpacing(target['spacing'])
            filt.SetOutputDirection([v for row in target['direction'] for v in row])
            filt.SetTransform(transform)
            filt.SetInterpolator(interpolator)
            filt.SetOutputPixelType(sitk.sitkFloat64)
            filt.SetDefaultPixelValue(-777.)
            filt.SetNumberOfWorkUnits(1)
            output = filt.Execute(image)
            expected = [output[x, y, z] for z in range(target['size'][2])
                        for y in range(target['size'][1]) for x in range(target['size'][0])]
            cases.append(dict(name=name + '_' + window, window=window,
                              source=dict(source, data=data), target=target,
                              matrix=matrix, offset=offset, padding=-777.,
                              expected=expected, inside=[v != -777. for v in expected]))
    return dict(format_version=1, simpleitk_version=version, itk_version=itk_version,
                input_pixel_type='float32', output_pixel_type='float64',
                data_order='x fastest, then y, then z', work_units=1, cases=cases)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=Path, default=Path(__file__).with_name('oracles.json'))
    args = parser.parse_args()
    args.output.write_text(json.dumps(generate(), indent=2, allow_nan=False) + '\n')
