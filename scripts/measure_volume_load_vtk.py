"""Opt-in VTK reference for TestVolumeLoadCampaign's own 48-file CT fixture.

Measures native DICOM directory reading and a 512x512 linear scalar reslice,
excluding Python startup, output verification, hashing and report writing.
No fixture, application or rendering fallback is installed by this script.
"""

import argparse
import gc
import hashlib
import json
import platform
import time
from pathlib import Path

import numpy as np
import vtk
from vtk.util.numpy_support import vtk_to_numpy


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixtures", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    args = parser.parse_args()
    paths = sorted(args.fixtures.glob("slice-*.dcm"))
    if len(paths) != 48:
        raise ValueError("expected the explicit 48-file campaign fixture")
    fixture_hashes = [hashlib.sha256(path.read_bytes()).hexdigest() for path in paths]
    vtk.vtkMultiThreader.SetGlobalDefaultNumberOfThreads(4)
    samples = []
    hashes = set()
    worst_error = 0.0
    y, x = np.mgrid[0:512, 0:512]
    expected = (100 + 300 * x / 511) + 2 * (100 + 300 * y / 511) + 3 * (10 + 20 * x / 511 + 5 * y / 511)
    for repetition in range(8):
        gc.collect()
        started = time.perf_counter_ns()
        reader = vtk.vtkDICOMImageReader()
        reader.SetDirectoryName(str(args.fixtures))
        reader.Update()
        loaded = time.perf_counter_ns()
        if reader.GetErrorCode() != 0:
            raise RuntimeError(f"VTK DICOM read error {reader.GetErrorCode()}")
        image = reader.GetOutput()
        if image.GetDimensions() != (512, 512, 48) or image.GetSpacing() != (1.0, 1.0, 1.0):
            raise ValueError("unexpected fixture dimensions or spacing")
        # This VTK reader reverses the fixture's Y and Z axes. Map the same
        # patient-space plane into those index coordinates without copying or
        # normalizing its volume before the timed reslice.
        axes = vtk.vtkMatrix4x4()
        axes.Identity()
        for row, value in enumerate((300 / 511, 0, -20 / 511)):
            axes.SetElement(row, 0, value)
        for row, value in enumerate((0, -300 / 511, -5 / 511)):
            axes.SetElement(row, 1, value)
        for row, value in enumerate((100, 411, 37)):
            axes.SetElement(row, 3, value)
        reslice = vtk.vtkImageReslice()
        reslice.SetInputData(image)
        reslice.SetResliceAxes(axes)
        reslice.SetOutputOrigin(0, 0, 0)
        reslice.SetOutputSpacing(1, 1, 1)
        reslice.SetOutputExtent(0, 511, 0, 511, 0, 0)
        reslice.SetOutputScalarType(vtk.VTK_DOUBLE)
        reslice.SetInterpolationModeToLinear()
        reslice.SetEnableSMP(False)
        reslice.SetNumberOfThreads(4)
        reslice.Update()
        ended = time.perf_counter_ns()
        if repetition:
            samples.append({"LoadMS": (loaded - started) / 1e6, "ResliceMS": (ended - loaded) / 1e6, "TotalMS": (ended - started) / 1e6})
        values = vtk_to_numpy(image.GetPointData().GetScalars()).reshape(48, 512, 512)
        normalized = np.ascontiguousarray(values[::-1, ::-1, :], dtype="<u2")
        hashes.add(hashlib.sha256(normalized.tobytes()).hexdigest())
        scalar = vtk_to_numpy(reslice.GetOutput().GetPointData().GetScalars()).reshape(512, 512)
        error = float(np.max(np.abs(scalar - expected)))
        worst_error = max(worst_error, error)
        # VTK linear weights are quantized; qualify this analytical plane with
        # an explicit absolute stored-value tolerance, not byte equivalence.
        if error > 1e-4:
            raise ValueError(f"VTK scalar reslice differs from analytical plane: {error}")
    if hashes != {"194034ce795e8429aa2096763a20cd520c9c31e093557bc4dd27b07d1d87093b"}:
        raise ValueError(f"VTK canonical CT values differ: {hashes}")
    report = {
        "OS": platform.platform(), "Architecture": platform.machine(), "Python": platform.python_version(),
        "VTK": vtk.vtkVersion.GetVTKVersion(), "SMPBackend": vtk.vtkSMPTools.GetBackend(), "ResliceThreads": 4,
        "Clock": time.get_clock_info("perf_counter").implementation,
        "PayloadSHA256": hashes.pop(), "MaxScalarAbsoluteError": worst_error,
        "FixtureSHA256": fixture_hashes, "Samples": samples,
    }
    args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"qualified 48-file VTK volume and scalar reslice; report={args.report}")


if __name__ == "__main__":
    main()
