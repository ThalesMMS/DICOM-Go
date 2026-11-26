"""Original synthetic halfway-registration comparison with pinned native oracles."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import itk
import numpy as np

if itk.Version.GetITKVersion() != "5.4.5" or np.__version__ != "2.2.6":
    raise RuntimeError("required ITK 5.4.5 and NumPy 2.2.6")
itk.MultiThreaderBase.SetGlobalDefaultNumberOfThreads(1)
case = json.load(sys.stdin)
Image = itk.Image[itk.F, 3]

def image(name):
    result = itk.image_from_array(np.asarray(case[name], dtype=np.float32).reshape(tuple(reversed(case["size"]))))
    result.SetSpacing(tuple(case["spacing"]))
    result.SetOrigin(tuple(case["origin"]))
    result.SetDirection(itk.matrix_from_array(np.asarray(case["direction"], dtype=np.float64)))
    return result

fixed, moving = image("fixed"), image("moving")
metric = itk.ANTSNeighborhoodCorrelationImageToImageMetricv4[Image, Image].New()
metric.SetRadius(tuple(case["radius"]))
metric.SetUseFixedImageGradientFilter(False)
metric.SetUseMovingImageGradientFilter(False)
method = itk.SyNImageRegistrationMethod[Image, Image].New(FixedImage=fixed, MovingImage=moving, Metric=metric)
method.SetNumberOfLevels(1)
method.SetNumberOfIterationsPerLevel([case["iterations"]])
method.SetShrinkFactorsPerLevel([1])
method.SetSmoothingSigmasPerLevel([0])
method.SetLearningRate(case["step"])
method.SetGaussianSmoothingVarianceForTheUpdateField(case["update_variance"])
method.SetGaussianSmoothingVarianceForTheTotalField(case["total_variance"])
method.SetConvergenceThreshold(1e-9)
method.SetConvergenceWindowSize(10)
start = time.perf_counter()
method.Update()
elapsed = time.perf_counter() - start
transform = method.GetOutput().Get()

def describe(transform, elapsed):
    field = transform.GetDisplacementField()
    array = np.asarray(itk.array_from_image(field))
    # Independent physical Jacobian of the sampled field at the grid nodes.
    gradient = np.empty(array.shape[:-1] + (3, 3), dtype=np.float64)
    for component in range(3):
        for axis in range(3):
            gradient[..., component, axis] = np.gradient(array[..., component], case["spacing"][axis], axis=2-axis)
    determinant = np.linalg.det(np.eye(3) + gradient)
    return {"elapsed_seconds": elapsed, "landmarks": [list(transform.TransformPoint(tuple(p))) for p in case["landmarks"]],
            "jacobian_min": float(determinant.min()), "folded_nodes": int(np.count_nonzero(determinant <= 0)),
            "size": list(itk.size(field)), "spacing": list(field.GetSpacing()), "origin": list(field.GetOrigin())}

itk_result = describe(transform, elapsed)
ants = Path(sys.argv[1]).resolve()
version = subprocess.check_output([str(ants), "--version"], text=True, timeout=30).strip()
if "ANTs Version: 2.6.5-" not in version:
    raise RuntimeError("required ANTs 2.6.5")
with tempfile.TemporaryDirectory(prefix="syn-reference-") as temp:
    directory = Path(temp)
    fpath, mpath = directory / "fixed.nii.gz", directory / "moving.nii.gz"
    itk.imwrite(fixed, str(fpath)); itk.imwrite(moving, str(mpath))
    prefix = directory / "ants"
    arguments = [str(ants), "--dimensionality", "3", "--float", "1", "--output", str(prefix),
                 "--interpolation", "Linear", "--transform", f"SyN[{case['step']},{case['update_variance']},{case['total_variance']}]",
                 "--metric", f"CC[{fpath},{mpath},1,{case['radius'][0]}]", "--convergence", f"[{case['iterations']},1e-9,10]",
                 "--shrink-factors", "1", "--smoothing-sigmas", "0", "--use-histogram-matching", "0"]
    environment = dict(os.environ, ITK_GLOBAL_DEFAULT_NUMBER_OF_THREADS="1")
    start = time.perf_counter()
    run = subprocess.run(arguments, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment, timeout=120)
    elapsed = time.perf_counter() - start
    if run.returncode:
        raise RuntimeError(f"ANTs failed {run.returncode}: {run.stdout}\n{run.stderr}")
    warp_path = directory / "ants0Warp.nii.gz"
    if not warp_path.is_file():
        raise RuntimeError(f"ANTs did not emit the declared single-stage warp: {list(directory.iterdir())}")
    field = itk.imread(str(warp_path), pixel_type=itk.Vector[itk.F, 3])
    ants_transform = itk.DisplacementFieldTransform[itk.F, 3].New(DisplacementField=field)
    ants_result = describe(ants_transform, elapsed)
json.dump({"itk_version": itk.Version.GetITKVersion(), "ants_version": version, "itk": itk_result, "ants": ants_result}, sys.stdout, allow_nan=False)
