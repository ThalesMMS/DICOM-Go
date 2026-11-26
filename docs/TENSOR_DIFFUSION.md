# Explicit structure-tensor diffusion

`imagefilter.TensorAnisotropicDiffusion` provides explicit CED and EED methods
on dense scalar 2D/3D `Volume` data. These are derived research estimates, with
owned output and recorded parameters. Existing Perona–Malik, MCDE and application
defaults remain separate. No modality, noise, clinical or viewer preset is
recommended by this qualification. This CPU image-processing operation does not
change Twin's GPU-only 3D rendering policy.

## Domain, units and spectral model

Choose `Method: imagefilter.CoherenceEnhancing` (CED) or
`Method: imagefilter.EdgeEnhancing` (EED). Two or three axes must have more than
one sample; 2D may occupy any coordinate plane. One-dimensional/isolated grids,
irregular spacing, multichannel data, nonfinite samples and masks are outside this
profile. Active spacing must be in `[1e-4,1e4]` mm; inactive spacing is finite and
nonnegative. Absolute input intensity must not exceed `1e30`, an explicit
arithmetic admission bound, not a modality range.

For each iteration, using physical centered differences with replicated end
samples, form

```
u_sigma = G_sigma * u
J = G_rho * (grad(u_sigma) grad(u_sigma)^T).
```

`SigmaMM` is the intensity smoothing standard deviation per axis;
`RhoMM` is the structure integration standard deviation per axis, both in mm.
Zero disables smoothing on that axis. The independent sampled Gaussian uses a
normalized positive kernel truncated at `ceil(3*scale/spacing)` samples and
replicated borders. Scales must be finite in `[0,1e4]` mm; radius greater than
64 is a resource rejection. Singleton axes are ignored. The centered gradient
has half the interior ramp slope at the replicated boundary. Its Nyquist
response vanishes, so undersampled structures cannot be recovered from that
gradient. No scale-normalized derivative or automatic noise threshold is used.

Let `mu_i` be J's eigenvalues on the active coordinate subspace. D has the same
eigenvectors and these eigenvalues:

```
CED: lambda_i = alpha + (1-alpha) exp(-(threshold/(mu_max-mu_i))^m)
EED: lambda_i = 1 - (1-alpha) exp(-(threshold/(mu_i-mu_min))^m).
```

At a zero gap the exponential is zero. Thus CED tends to `alpha*I` on a
degenerate tensor and smooths more in directions of low structure variation;
EED tends to `I` on a degenerate tensor and suppresses directions of high
variation. Eigenvalues lie in `[alpha,1]`. In 3D the generalized CED gap family
can enhance both low-variation tangent directions of a plane; it is not limited
to a single fiber direction. Repeated eigenvalues have identical transformed
weights, so arbitrary vectors in their eigenspace cannot change D. The 2D
implementation uses analytic projectors; 3D uses at most 16 Jacobi sweeps with
relative off-diagonal threshold `1e-15` and tiny negative eigenvalues rounded
to zero. Analytical eigenpair reconstruction is tested through scales `1e-20`
to `1e40` and eigenvalue repetitions, with relative residual tolerance `2e-14`.
This is relative tensor accuracy, not uniform accuracy of every spectral gap.
When an eigenvalue difference is at or below float64 roundoff/solver tolerance
relative to J's magnitude, its orientation and contrast cannot be resolved
reliably; an arbitrarily small Threshold does not restore that lost precision.
The broad arithmetic admission range is not a guarantee of resolving such gaps.

`Alpha` is mandatory in `[1e-6,1]`; `Alpha=1` is isotropic heat diffusion.
`Threshold` is mandatory in `[1e-24,1e24]`, in `(input intensity/mm)^2`.
Exponent zero selects 2, with integers 1–8 admitted. Scaling intensities by
`c` requires threshold multiplied by `c^2` to retain the same response.
These spectral transforms follow the public
[Weickert CED family](https://www-public.imtbs-tsp.eu/~rougon/IMA4509/Exam/Sujets/Weickert-IVC-1999.pdf)
and the generalized CED/EED equations in the
[ITK public contract](https://github.com/InsightSoftwareConsortium/ITK/blob/be770907fc62a2ae0b1395a1f092465d7ab9b535/Modules/Filtering/AnisotropicDiffusionLBR/include/itkCoherenceEnhancingDiffusionImageFilter.h#L38-L55).
Implementation and the discrete operator below were independently derived;
they do not reproduce ITK's LBR solver or its recursive Gaussian approximation.

## Discretization, borders and stability proof

For active dimension d and each orthant `s in {-1,+1}^d`, define at node p

```
(G_s u)_a(p) = (u(p+s_a*e_a)-u(p))/(s_a*h_a)
```

when that neighbor exists, and zero otherwise. With D frozen for this step,
the energy and operator are

```
E_D(u) = (1/(2*2^d)) sum_s (G_s u)^T D (G_s u)
A_D = (1/2^d) sum_s G_s^T D G_s
u_next = u - dt*A_D*u.
```

Absent differences produce the discrete natural/no-flux boundary; no flux is
sent to nonexistent nodes. The implementation gathers the equivalent averaged
diagonal edge coefficients and centered cross terms, so worker writes never
overlap. Orthant averaging preserves axis permutation and reflection symmetry.
It avoids the grid parity decoupling of a solely centered-gradient energy.

Every `G_s,a` is a finite directed axial difference divided by `h_a`, with norm
at most `2/h_a`: use `(v-w)^2 <= 2v^2+2w^2`, counting each node at most twice
on a line. Therefore `||G_s||^2 <= 4 sum_a h_a^-2`. Each block D is symmetric
positive definite with eigenvalues at most 1, so A is symmetric positive
semidefinite and `||A|| <= 4 sum_a h_a^-2`. Its constant vector is in the
nullspace. The update's eigenvalues are between -1 and 1 for

```
0 < dt <= 1/(2*sum_active_axes(1/h_a^2)).
```

Hence each frozen step conserves the mean and does not increase squared L2;
recomputing D for the next step preserves this per-step argument. This is a
bound for this actual operator on anisotropic spacing, not an assumption based
on the isotropic Laplacian. It describes exact arithmetic. Float64 eigensystems,
reductions and final float32 rounding introduce small deviations; qualification
allows mean drift `2e-12`, relative L2 increase `3e-14`, and oracle tolerances
below. TimeStep zero selects 90% of the bound. A value even one representable
float64 above the calculated bound is rejected. Iterations zero selects 5;
there is no automatic stop and total physical diffusion time is `iterations*dt`
in mm². A change of all physical lengths by c requires time multiplied by c²
and threshold divided by c².

The operator is not a monotone LBR stencil. Cross coefficients may be negative;
L2 stability does not imply preservation of minimum, maximum, positivity or
topology. No clamping conceals the effect. For an independent fixed oblique
tensor `D=.05I+.95vv^T`, with v the unit all-ones vector and a unit impulse at
the center, the first step at the stability limit produced:

| Grid, 1 mm spacing | Step (mm²) | Minimum | Maximum | Squared L2, from 1 |
| --- | ---: | ---: | ---: | ---: |
| 17×17 | .25 | -.059375 | .475 | .3086328125 |
| 11×11×11 | 1/6 | -.026388888889 | .633333333333 | .431875 |

Forty steps in both cases retained mass 1 within `3e-14` and decreased L2 at
every step. This quantitative undershoot is a retained limitation. Applications
requiring positive or maximum-principle-preserving diffusion must select a
different qualified method.

## Ownership, cancellation and budgets

All input samples and the optional opaque `SourceReference` (maximum 4096 bytes)
must remain immutable during execution. The library allocates its own float64
iterates and tensor, returns detached float32 samples, and clones the identifier.
Metadata records derived domain, method, borders, stencil, geometry, scales,
threshold/alpha/exponent, physical time, budgets, residual RMS, means and L2.
`SourceSHA256` covers ordered source float32 little-endian bytes; geometry is
recorded separately. Statistical values describe the pre-rounding float64
iterate. Derived estimates are not acquired quantitative measurements.

Admission precedes voxel/kernel allocation. Negative or above-ceiling limits
are invalid. Zero selects the finite defaults:

| Budget | Default | Hard ceiling |
| --- | ---: | ---: |
| Voxels | 1,048,576 | 16,777,216 |
| Iterations | 128 | 1000 |
| Scratch payload | 128 MiB | 2 GiB |
| Owned working payload | 160 MiB | 2 GiB |
| Conservative stencil sample reads | 2,000,000,000 | 50,000,000,000 |

Four float64 intensity/scratch arrays plus six float64 tensor components cost
`80*n` bytes. Gaussian kernel payload K is eight times the sum of all admitted
kernel widths. Scratch is `80*n+K`; working adds `4*n` for output plus cloned
identifier bytes. Source, fixed structs/closures, slice headers, SHA state,
allocator/runtime and goroutine stacks are excluded. Logical payload admission
is not a process RSS cap. This profile is dense and does not offer slab streaming.
The sample-read bound is
`n*(iterations*(sum_sigma_widths+6*sum_rho_widths+128)+8)`; 128 conservatively
covers tensor construction/conversion and the gathered update, and widths cover
each separable convolution pass. This is a work admission surrogate, not an
operation count or elapsed-time guarantee.

Workers zero uses the process-wide shared concurrency budget, capped at 64;
explicit values 1–64 bound this call. Per-voxel work has fixed stack state and
allocates no worker-sized volume or tensor. Results are worker-independent.
Context is checked during admission/hash/output every 256 samples, between
passes and before every claimed voxel in parallel passes. Copies and the bounded
kernel/eigen work finish before the next check. All helpers join on return.
Cancellation and every error return nil. Identifiable errors are
`ErrTensorDiffusionInput`, `ErrTensorDiffusionLimit`,
`ErrTensorDiffusionNumeric`, and the context error. No partial result is returned
as completed.

## Independent local numerical and task evidence


The independent end-to-end 2D oracle uses direct product-kernel convolution,
angle eigenvectors and orthant scattering. It shares declared sampling, borders,
scales, transfer equations and dt, with three nonlinear steps at `.03` mm²;
final comparison tolerance is `2e-7` input units for float32 rounding. A separate
2D/3D scatter oracle matches the gathered stencil within `2e-14` on varying
analytic positive tensors. Isotropic alpha=1 matches independent physical heat
updates within `2e-7`. Constant/line/edge/junction/oblique phantoms at the exact
physical stability bound, anisotropic spacing, axis/reflection symmetries,
length/intensity scaling, rotated/repeated eigenvalues, immutable ownership,
exact-budget admission and cancellation were checked. Axis symmetries tolerate
`5e-6` float32 units; worker outputs are bit-identical. No external ITK output
identity is claimed: its stencil, Gaussian approximation and time integration
are different.

Reproducible controls use 33×33 at spacing (.7,1.3) mm, xorshift32 seed1510
uniform noise in [-.5,.5], and these truths: constant 3; vertical Gaussian line
`4*exp(-(x-16)^2/2)`; a step of 4 at x=16; crossing Gaussian lines of peak 8.
All methods use 20 steps at `.04` mm² (total `.8` mm²), workers4. CED/EED use
sigma(.5,.5), rho(1,1) mm, alpha.05, threshold.3, exponent2. Perona–Malik and
MCDE use conductance1 relative to each method's current RMS gradient. Thus time
matches but conductance models differ; these are declared experiments, not
optimal parameter fits or clinical comparisons.

| Truth | Method | MSE versus truth | RMS change versus input | Adjacent-y RMS |
| --- | --- | ---: | ---: | ---: |
| Constant | Input | .08598646 | 0 | .4130320 |
| Constant | CED | .05827805 | .06020695 | .3339251 |
| Constant | EED | .00415880 | .26948584 | .0505898 |
| Constant | Perona–Malik | .03234214 | .16378746 | .2216007 |
| Constant | MCDE | .01607975 | .22225521 | .1387444 |
| Line | CED | .05326597 | .11275286 | .2988472 |
| Line | EED | .01485987 | .26733048 | .0648697 |
| Line | Perona–Malik | .03227847 | .27709021 | .1683056 |
| Line | MCDE | .05082754 | .31607222 | .1291171 |
| Edge | CED | .05855583 | .11669339 | .3066817 |
| Edge | EED | .01594104 | .27471343 | .0614191 |
| Edge | Perona–Malik | .00562228 | .26332869 | .0687890 |
| Edge | MCDE | .00674072 | .27286391 | .0491045 |
| Junction | CED | .05160451 | .16117417 | .6562908 |
| Junction | EED | .02635861 | .28696937 | .5687909 |
| Junction | Perona–Malik | .04803236 | .29474134 | .5838746 |
| Junction | MCDE | .08320311 | .35956488 | .5843555 |

Input MSE is .08598646 for each truth. The clean vertical line's center mean is
4; noisy input3.93233 becomes CED3.70566, EED3.56520, PM3.56653, MCDE3.00661.
The clean junction peak8 (noisy7.54363) becomes CED6.60350, EED6.02721,
PM6.63981, MCDE6.35472. EED reduces this noise effectively while attenuating
structures; Perona–Malik has the lowest edge MSE in this control. Adjacent-y RMS
contains real signal in the junction, so its reduction is not pure denoising.
These controls neither rank arbitrary anatomy nor justify automatic substitution.

## Measured cost and whole-process peak memory

Four benchmark cells use the oblique synthetic phantom, spacing(.7,1.3,2.1),
sigma(.5,.5,.5), rho(1,1,1) mm, alpha.05, threshold.3, default step and ten
iterations. Each grid has 4096 samples. Three 100ms benchmark samples per cell
include admission, hash, tensor recomputation, smoothing, diffusion, diagnostics
and output allocation. Setup is excluded. Each RSS value is a separate fresh
test process running exactly one benchmark operation under `/usr/bin/time -l`;
it includes the binary, runtime, test harness and input as well as filter memory.

| Method / grid | Time range (ms/op), median | Allocated bytes/op range | Process peak RSS bytes |
| --- | ---: | ---: | ---: |
| CED / 64² | 33.210–38.777, 36.232 | 394,956–403,342 | 10,092,544 |
| CED / 16³ | 34.818–57.320, 52.471 | 416,344–416,408 | 10,305,536 |
| EED / 64² | 30.169–37.124, 31.354 | 394,956–394,992 | 10,158,080 |
| EED / 16³ | 51.090–58.282, 54.517 | 416,352–416,640 | 10,371,072 |

These short, variable samples are local evidence, not a performance guarantee
or a comparison to other libraries. Logical payloads and allocations are
distinct from peak RSS; larger-volume peaks were not measured. Reproduce from
`dicom-go/`:

```sh
GOMAXPROCS=4 go test -p 2 ./imagefilter -run '^TestTensorDiffusion' -count=1 -v
GOMAXPROCS=4 go test -race -p 2 ./imagefilter \
  -run '^TestTensorDiffusion(CancellationAndConcurrentCalls|SymmetryScalingWorkers)$' -count=1
GOMAXPROCS=4 go test -p 2 ./imagefilter -run '^$' \
  -bench '^BenchmarkTensorDiffusionCost$' -benchtime=100ms -count=3
GOMAXPROCS=4 go test -p 2 -c ./imagefilter -o /tmp/tensor-imagefilter.test
GOMAXPROCS=4 /usr/bin/time -l /tmp/tensor-imagefilter.test -test.run='^$' \
  -test.bench='^BenchmarkTensorDiffusionCost$/^CED$/^2D64$' -test.benchtime=1x
```

For the other RSS cells substitute EED and/or 3D16 in the last selector.
Qualification covers synthetic numerical behavior and bounded local execution;
it does not cover acquisition fidelity, external LBR interoperability,
clinical efficacy, arbitrary topology, positivity or GPU rendering.
