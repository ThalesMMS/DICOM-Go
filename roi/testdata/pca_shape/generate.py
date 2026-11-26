"""Project-authored synthetic spheres and independent NumPy PCA (no PHI).

Training and held-out shapes/poses are fixed below, before running the Go gate.
Run with NumPy; emits a compressed JSON numerical oracle, not patient models.
"""
import gzip
import hashlib
import json
from pathlib import Path
import numpy as np

here = Path(__file__).resolve().parent
dims = [11, 9, 7]
spacing = [0.8, 1.2, 1.4]
angle = 0.3
direction = np.array([[np.cos(angle), -np.sin(angle), 0],
                      [np.sin(angle), np.cos(angle), 0], [0, 0, 1]])
origin = [10.0, -20.0, 30.0]
zz, yy, xx = np.indices(dims[::-1], dtype=float)
local = np.column_stack(((xx.ravel()-5)*spacing[0],
                         (yy.ravel()-4)*spacing[1],
                         (zz.ravel()-3)*spacing[2]))
geometry = dict(dimensions=dims, spacingMM=spacing, originMM=origin,
                direction=direction.ravel().tolist())

def sphere(radius, center):
    return np.linalg.norm(local-np.array(center), axis=1)-radius

training_specs = [(2.0, [-.4, -.2, 0]), (2.3, [.4, -.2, 0]),
                  (2.6, [-.4, .2, 0]), (2.9, [.4, .2, 0]),
                  (3.2, [0, 0, 0]), (2.6, [0, 0, 0]),
                  (2.3, [0, .3, 0]), (2.9, [0, -.3, 0])]
training = np.array([sphere(r, c) for r, c in training_specs])
mean = training.mean(axis=0)
_, singular, right = np.linalg.svd(training-mean, full_matrices=False)
rank = 6
components = right[:rank].copy()
for row in components:
    if row[np.argmax(np.abs(row))] < 0:
        row *= -1
sigma = singular[:rank]/np.sqrt(len(training)-1)
held_specs = [(2.45, [.1, -.05, .1]), (2.75, [-.1, .1, -.05])]
held_fields = [sphere(r, c) for r, c in held_specs]
held_fields.append(np.linalg.norm(local / np.array([1.7, 1, 1]), axis=1)-2.6)
held = []
for i, field in enumerate(held_fields):
    coefficients = (field-mean) @ components.T / sigma
    reconstruction = mean + (coefficients*sigma) @ components
    held.append(dict(id=f"held-{i}", fieldMM=field.tolist(),
                     coefficientsSD=coefficients.tolist(),
                     reconstructionMM=reconstruction.tolist(),
                     residualRMSMM=float(np.sqrt(np.mean((field-reconstruction)**2)))))
receipt = dict(schema=1, engine="NumPy", version=np.__version__,
               convention="negative-inside-mm", geometry=geometry,
               training=[dict(id=f"train-{i}", radiusMM=r, centerLocalMM=c,
                              fieldMM=training[i].tolist())
                         for i, (r,c) in enumerate(training_specs)],
               retainedRank=rank, meanMM=mean.tolist(),
               components=components.tolist(), standardDeviationsMM=sigma.tolist(),
               heldOut=held, generatorSHA256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest())
packed = gzip.compress((json.dumps(receipt, separators=(",", ":"))+"\n").encode(), mtime=0)
(here / "numpy.json.gz").write_bytes(packed)
print(f"NumPy {np.__version__}: {len(training)} training, {len(held)} held-out, rank {rank}, "
      f"{len(mean)} voxels; SHA256 {hashlib.sha256(packed).hexdigest()}")
for h in held:
    print(h["id"], "residual-mm", h["residualRMSMM"], "max-coefficient-SD",
          max(abs(v) for v in h["coefficientsSD"]))
