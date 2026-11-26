"""Pinned optional metric oracle; original synthetic fixtures arrive on stdin."""
import json
import sys
import itk
import numpy as np

if itk.Version.GetITKVersion() != "5.4.5" or np.__version__ != "2.2.6":
    raise RuntimeError("required ITK 5.4.5 and NumPy 2.2.6")
itk.MultiThreaderBase.SetGlobalDefaultNumberOfThreads(1)
Image = itk.Image[itk.F, 3]
Metric = itk.ANTSNeighborhoodCorrelationImageToImageMetricv4[Image, Image]

def image(case, name):
    array = np.asarray(case[name], dtype=np.float32).reshape(tuple(reversed(case["size"])))
    result = itk.image_from_array(array)
    result.SetSpacing(tuple(case["spacing"]))
    result.SetOrigin(tuple(case["origin"]))
    result.SetDirection(itk.matrix_from_array(np.asarray(case["direction"], dtype=np.float64)))
    return result

results = []
for case in json.load(sys.stdin):
    fixed, moving = image(case, "fixed"), image(case, "moving")
    transform = itk.TranslationTransform[itk.D, 3].New()
    transform.SetOffset(tuple(case["translation"]))
    metric = Metric.New(FixedImage=fixed, MovingImage=moving)
    metric.SetFixedTransform(itk.IdentityTransform[itk.D, 3].New())
    metric.SetMovingTransform(transform)
    metric.SetVirtualDomainFromImage(fixed)
    metric.SetRadius(tuple(case["radius"]))
    metric.SetUseFixedImageGradientFilter(False)
    metric.SetUseMovingImageGradientFilter(False)
    metric.Initialize()
    results.append({"name": case["name"], "value": metric.GetValue(), "valid_centers": metric.GetNumberOfValidPoints()})
json.dump({"itk": itk.Version.GetITKVersion(), "numpy": np.__version__, "results": results}, sys.stdout, allow_nan=False)
