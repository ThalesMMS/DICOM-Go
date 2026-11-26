"""Opt-in mathematical oracle and plot for the fixed 1548 campaign.

Own equations/data; NumPy supplies an independent FFT. This is not ITK parity:
ITK's referenced oriented log-Gabor bank has a different transfer contract.
Usage: python compare_numpy.py ABSOLUTE_TEMP_CAMPAIGN_DIRECTORY
"""
import hashlib
import json
import pathlib
import sys

import numpy as np

directory = pathlib.Path(sys.argv[1]).resolve()
plan_path = pathlib.Path(__file__).with_name("plan.json")
plan = json.loads(plan_path.read_text(encoding="utf-8"))
report = json.loads((directory / "campaign.json").read_text(encoding="utf-8"))
assert hashlib.sha256(plan_path.read_bytes()).hexdigest() == report["PlanSHA256"]
w, h, _ = plan["dims"]
sx, sy, _ = plan["spacing_mm"]
scales = plan["scales_mm"]
mx, my = int(np.ceil(6 * scales[-1] / sx)), int(np.ceil(6 * scales[-1] / sy))
pw, ph = 1 << (w + 2 * mx - 1).bit_length(), 1 << (h + 2 * my - 1).bit_length()
wx, wy = np.meshgrid(2 * np.pi * np.fft.fftfreq(pw, sx), 2 * np.pi * np.fft.fftfreq(ph, sy))
radius = np.hypot(wx, wy)
rx_multiplier = np.divide(-1j * wx, radius, out=np.zeros_like(wx, dtype=complex), where=radius > 0)
ry_multiplier = np.divide(-1j * wy, radius, out=np.zeros_like(wy, dtype=complex), where=radius > 0)
rx_multiplier[:, pw // 2] = 0
ry_multiplier[ph // 2, :] = 0
oracle = {"numpy": np.__version__, "python": sys.version, "plan_sha256": report["PlanSHA256"], "cases": []}
images = []
for index, case in enumerate(report["Cases"]):
    source = np.fromfile(directory / f"case-{index}-source.f32", dtype="<f4").reshape(h, w).astype(float)
    got = np.fromfile(directory / f"case-{index}-phase.f32", dtype="<f4").reshape(h, w)
    baseline = np.fromfile(directory / f"case-{index}-frangi.f32", dtype="<f4").reshape(h, w)
    padded = np.pad(source, ((my, ph - h - my), (mx, pw - w - mx)), mode="reflect")
    spectrum = np.fft.fft2(padded)
    amplitude, energy = np.zeros_like(source), np.zeros_like(source)
    previous = 0.0
    reconstructed = np.zeros_like(source)
    for sigma in scales:
        weight = np.exp(-0.5 * previous ** 2 * radius ** 2) - np.exp(-0.5 * sigma ** 2 * radius ** 2)
        band = spectrum * weight
        even = np.fft.ifft2(band).real[my:my+h, mx:mx+w]
        oddx = np.fft.ifft2(band * rx_multiplier).real[my:my+h, mx:mx+w]
        oddy = np.fft.ifft2(band * ry_multiplier).real[my:my+h, mx:mx+w]
        odd = np.hypot(oddx, oddy)
        amplitude += np.hypot(even, odd)
        energy += np.maximum(even - odd - plan["phase"]["noise_threshold"], 0)
        reconstructed += even.astype(np.float32).astype(float)
        previous = sigma
    reconstructed += np.fft.ifft2(spectrum * np.exp(-0.5 * previous ** 2 * radius ** 2)).real[my:my+h, mx:mx+w].astype(np.float32)
    expected = np.divide(energy, amplitude, out=np.zeros_like(amplitude), where=amplitude > plan["phase"]["amplitude_floor"]).astype(np.float32)
    error = np.abs(got - expected)
    reconstruction_error = float(np.max(np.abs(reconstructed.astype(np.float32) - source)))
    assert float(error.max()) <= 2e-6
    assert reconstruction_error <= 2e-6 * max(1.0, float(np.abs(source).max()))
    oracle["cases"].append({"index": index, "maximum_phase_error": float(error.max()), "rms_phase_error": float(np.sqrt(np.mean(error ** 2))), "maximum_reconstruction_error": reconstruction_error})
    images.append((source, got, baseline))
(directory / "numpy-oracle.json").write_text(json.dumps(oracle, indent=2) + "\n", encoding="utf-8")

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

fig, axes = plt.subplots(5, 3, figsize=(8, 11), constrained_layout=True)
for index, (source, phase, baseline) in enumerate(images):
    for column, data in enumerate((source, phase, baseline)):
        axes[index, column].imshow(data, cmap="gray" if column == 0 else "viridis", vmin=None if column == 0 else 0, vmax=None if column == 0 else 1, interpolation="nearest")
        axes[index, column].set_xticks([])
        axes[index, column].set_yticks([])
    case = report["Cases"][index]
    axes[index, 0].set_ylabel(f"seed {case['Parameters']['seed']}\n{'held out' if case['HeldOut'] else 'development'}", fontsize=9)
for column, title in enumerate(("Own input (per-image range)", "Experimental phase (0 to 1)", "Existing Frangi (0 to 1)")):
    axes[0, column].set_title(title, fontsize=10)
fig.suptitle("Fixed 2D ridge campaign: promotion gate failed", fontsize=12)
fig.savefig(directory / "comparison.png", dpi=140)
plt.close(fig)
print(json.dumps(oracle, indent=2))
