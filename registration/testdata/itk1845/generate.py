#!/usr/bin/env python3
"""Generate synthetic dense-group fixtures with public ITK and HDF5 APIs."""

import argparse
import hashlib
import json
import platform
from pathlib import Path

import h5py
import SimpleITK as sitk


def mixed_composite(path):
    angles = (0.0, -1.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 1.0)
    origin = (-12.0, 4.0, -8.0)
    spacing = (2.0, 2.5, 3.0)
    size = (10, 9, 8)
    domain = sitk.Image(size, sitk.sitkFloat32)
    domain.SetOrigin(origin)
    domain.SetSpacing(spacing)
    domain.SetDirection(angles)
    spline = sitk.BSplineTransformInitializer(domain, [2, 2, 2])
    spline.SetParameters(
        [((13 * i) % 29 - 14) / 1024 for i in range(spline.GetNumberOfParameters())]
    )
    vector_image = sitk.Image(size, sitk.sitkVectorFloat64, 3)
    vector_image.CopyInformation(domain)
    for z in range(size[2]):
        for y in range(size[1]):
            for x in range(size[0]):
                vector_image.SetPixel(
                    (x, y, z),
                    ((x - y) / 256, (z + y) / 512, (x + z - 4) / 256),
                )
    displacement = sitk.DisplacementFieldTransform(vector_image)
    affine = sitk.AffineTransform(3)
    affine.SetCenter((-21.0, 10.0, 1.0))
    affine.SetMatrix((1.025, 0.015625, 0.0, 0.0, 0.98, -0.03125, 0.0078125, 0.0, 1.01))
    affine.SetTranslation((0.125, -0.0625, 0.03125))
    nontranslations = {4: affine, 27: spline, 45: displacement}
    chain = sitk.CompositeTransform(3)
    for i in range(58):
        transform = nontranslations.get(i)
        if transform is None:
            transform = sitk.TranslationTransform(
                3, (((7 * i) % 13 - 6) / 512, ((5 * i) % 11 - 5) / 512, (i % 3) / 512)
            )
        chain.AddTransform(transform)
    sitk.WriteTransform(chain, str(path))
    restored = sitk.ReadTransform(str(path))
    if restored.GetNumberOfTransforms() != chain.GetNumberOfTransforms():
        raise ValueError("ITK changed the number of components")
    points = []
    for index in ((2.3, 1.4, 3.2), (4.2, 2.2, 4.4), (6.3, 4.1, 2.1), (13.2, 14.5, -4.2)):
        point = domain.TransformContinuousIndexToPhysicalPoint(index)
        expected = chain.TransformPoint(point)
        if restored.TransformPoint(point) != expected:
            raise ValueError("ITK write/read changed the mapping")
        # The public SimpleITK API exposes points. Estimate the position
        # Jacobian from those values away from interpolation boundaries.
        h = 1 / 65536
        jacobian = [[0.0] * 3 for _ in range(3)]
        for axis in range(3):
            forward, backward = list(point), list(point)
            forward[axis] += h
            backward[axis] -= h
            upper = restored.TransformPoint(forward)
            lower = restored.TransformPoint(backward)
            for row in range(3):
                jacobian[row][axis] = (upper[row] - lower[row]) / (2 * h)
        points.append({"input": point, "output": expected, "jacobian": jacobian})
    with h5py.File(path, "r") as file:
        hdf5_version = file["HDFVersion"][0].decode("ascii")
    return {
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
        "itk_hdf5_version": hdf5_version,
        "queue": [chain.GetNthTransform(i).GetName() for i in range(chain.GetNumberOfTransforms())],
        "points": points,
    }


def ordered_group(path):
    names = [f"g_{(i * 37) % 73:02d}" for i in range(73)]
    with h5py.File(path, "w", libver=("v108", "v108")) as file:
        group = file.create_group("ordered", track_order=True)
        for name in names:
            group.create_group(name)
        nonhard = file.create_group("nonhard")
        for index in range(9):
            nonhard.create_group(str(index))
        nonhard["alias"] = h5py.SoftLink("/nonhard/0")
    with h5py.File(path, "r") as file:
        if list(file["ordered"]) != names:
            raise ValueError("HDF5 changed indexed creation order")
    return {
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
        "names": names,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    manifest = {
        "simpleitk": sitk.Version_VersionString(),
        "itk": sitk.Version_ITKVersionString(),
        "h5py": h5py.__version__,
        "hdf5": h5py.version.hdf5_version,
        "python": platform.python_version(),
        "mixed": mixed_composite(args.output / "mixed.h5"),
        "ordered": ordered_group(args.output / "ordered.h5"),
    }
    (args.output / "fixtures.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
