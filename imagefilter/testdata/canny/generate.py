"""Authored geometric fixtures, public SimpleITK API only. No upstream data/code."""
import json, math, pathlib
import SimpleITK as sitk
import numpy as np
sitk.ProcessObject.SetGlobalDefaultNumberOfThreads(1)

def image(data, dims, spacing):
    im = sitk.GetImageFromArray(np.asarray(data, dtype=np.float32).reshape(tuple(reversed(dims[:2] if dims[2] == 1 else dims))))
    im.SetSpacing(spacing[:im.GetDimension()])
    return im

def values(dims, spacing, shape):
    center = [(n-1)/2 for n in dims]
    out = []
    for z in range(dims[2]):
        for y in range(dims[1]):
            for x in range(dims[0]):
                p = [(i-c)*h for i,c,h in zip((x,y,z),center,spacing)]
                r = math.sqrt(sum(v*v for v in p))
                inside = {'constant':False, 'step':p[0]<0, 'oblique':p[0]+.6*p[1]+.3*p[2]<0,
                          'ball':r<12, 'tube':math.hypot(p[0],p[1])<9,
                          'two':min(math.hypot(p[0]-9,p[1]),math.hypot(p[0]+9,p[1]))<5}[shape]
                out.append(100.0 if inside else 0.0)
    return out

def make_case(name,dims,spacing,shape,sigma=(1.,1.,1.),lower=5.,upper=10.):
    data=values(dims,spacing,shape)
    f=sitk.CannyEdgeDetectionImageFilter(); f.SetVariance([s*s for s in sigma[:2 if dims[2]==1 else 3]])
    f.SetLowerThreshold(lower); f.SetUpperThreshold(upper); f.SetMaximumError([.01]* (2 if dims[2]==1 else 3))
    edges=sitk.GetArrayFromImage(f.Execute(image(data,dims,spacing))).ravel()
    return dict(name=name,dims=dims,spacing=spacing,sigma=sigma,lower=lower,upper=upper,data=data,
                edges=[int(v!=0) for v in edges])

def main():
    cases=[make_case('constant',[41,39,1],[1.,1.,1.],'constant'),
           make_case('step',[41,39,1],[1.,1.,1.],'step'),
           make_case('oblique',[41,39,1],[1.,1.,1.],'oblique'),
           make_case('ring',[41,39,1],[1.,1.,1.],'ball'),
           make_case('two-components',[41,39,1],[1.,1.,1.],'two'),
           make_case('anisotropic-step',[41,39,1],[.7,1.5,1.],'step',(.9,1.2,1.)),
           make_case('plane',[29,27,25],[1.,1.,1.],'step'),
           make_case('sphere',[29,27,25],[1.,1.,1.],'ball'),
           make_case('tube',[29,27,25],[1.,1.,1.],'tube'),
           make_case('anisotropic-plane',[29,27,25],[.7,1.3,1.8],'oblique',(1.,1.,1.))]
    # Characterize units via public threshold sweeps: edge amplitude scales with
    # intensity; spacing is passed to the filter without index-space resampling.
    probes=[]
    for h,amplitude in ((1.,100.),(2.,100.),(1.,200.)):
        dims=[41,39,1]; spacing=[h,h,1.]; data=[amplitude if x<20 else 0. for y in range(39) for x in range(41)]
        counts=[]
        for t in (1.,5.,10.,20.,40.,80.):
            f=sitk.CannyEdgeDetectionImageFilter();f.SetVariance([1.,1.]);f.SetLowerThreshold(t);f.SetUpperThreshold(t)
            counts.append(int(np.count_nonzero(sitk.GetArrayFromImage(f.Execute(image(data,dims,spacing))))))
        probes.append(dict(spacing=h,amplitude=amplitude,thresholds=[1.,5.,10.,20.,40.,80.],counts=counts))
    document=dict(generator='authored geometry; public SimpleITK API',version=str(sitk.Version()),variance_units='mm^2',
                  criteria={'max_bidirectional_distance_in_max_spacing':1.,'constant_exact_empty':True,'components_equal':True},
                  unit_probes=probes,cases=cases)
    pathlib.Path(__file__).with_name('itk-5.4.json').write_text(json.dumps(document,separators=(',',':'),allow_nan=False)+'\n')
if __name__=='__main__':main()
