"""Authored features/initializations; public SimpleITK wrappers only."""
import json,math,pathlib
import SimpleITK as sitk
import numpy as np
sitk.ProcessObject.SetGlobalDefaultNumberOfThreads(1)
def image(data,dims,spacing):
    i=sitk.GetImageFromArray(np.asarray(data,dtype=np.float32).reshape(tuple(reversed(dims[:2] if dims[2]==1 else dims))))
    i.SetSpacing(spacing[:i.GetDimension()]);return i

def fixture(kind,dims,spacing,profile="original"):
    rmax=12.;rinit=9.; data=[];feature=[];initial=[]
    for z in range(dims[2]):
        for y in range(dims[1]):
            for x in range(dims[0]):
                r=math.sqrt(sum(((i-(n-1)/2)*h)**2 for i,n,h in zip((x,y,z),dims,spacing)))
                data.append(100. if r<rmax else 0.)
                feature.append((1. if r<rmax else 0.) if profile=="zero-barrier" else 1. if profile=="curvature-horizon" else 1./(1.+100.*math.exp(-((r-rmax)/.8)**2)))
                initial.append(-.5*min(spacing[:2 if dims[2]==1 else 3]) if r<rinit else .5*min(spacing[:2 if dims[2]==1 else 3]))
    f=sitk.ShapeDetectionLevelSetImageFilter() if kind=='shape' else sitk.CannySegmentationLevelSetImageFilter()
    iterations=3 if profile=="curvature-horizon" else 300
    f.SetNumberOfIterations(iterations);f.SetMaximumRMSError(.0005);f.SetCurvatureScaling(.2)
    propagation=1. if kind=="shape" and profile!="curvature-horizon" else 0.
    f.SetPropagationScaling(propagation)
    if kind=='canny':
        f.SetVariance(1.);f.SetThreshold(5.);f.SetAdvectionScaling(1.);f.SetReverseExpansionDirection(True)
    out=f.Execute(image(initial,dims,spacing),image(feature if kind=='shape' else data,dims,spacing))
    case=dict(name=kind+('-2d' if dims[2]==1 else '-3d'),kind=kind,dims=dims,spacing=spacing,data=data,feature=feature,
              initial=[int(v<0) for v in initial],mask=[int(v<0) for v in sitk.GetArrayFromImage(out).ravel()],
              iterations=f.GetElapsedIterations(),rms=f.GetRMSChange(),max_iterations=iterations,maximum_rms=.0005,
              propagation=propagation,curvature=.2,advection=1. if kind=='canny' else 0.,
              sigma=1.,threshold=5.,reverse_expansion=(kind=='canny'))
    if profile!="original":case.update(name=case["name"]+"-"+profile,profile=profile,curvature_convention="sum",stopping="fixed-iterations" if profile=="curvature-horizon" else "RMS-only")
    if kind=='canny':
        edges=f.GetCannyImage(); distance=sitk.DanielssonDistanceMap(sitk.Cast(edges,sitk.sitkUInt8),inputIsBinary=True,squaredDistance=False,useImageSpacing=True)
        # Public probes fix the unit relation a=D²/2; no access to private ITK fields.
        d=sitk.GetArrayFromImage(distance).ravel();case['edges']=[int(v!=0) for v in sitk.GetArrayFromImage(edges).ravel()]
        case['distance_mm']=[float(v) for v in d];case['half_squared_mm2']=[float(v)*float(v)/2 for v in d]
    return case

def main():
    cases=[fixture(k,d,s) for k in ('shape','canny') for d,s in (([41,41,1],[1.,1.,1.]),([29,29,29],[1.,1.,1.]))]
    document=dict(version=str(sitk.Version()),generator='authored radial features/masks, public SimpleITK APIs',
                  sign='negative inside; Canny ReverseExpansionDirection=true documented API mapping',
                  criteria={'minimum_dice':.95,'max_symmetric_boundary_distance_in_max_spacing':2.},cases=cases,
                  positive_cases=[fixture("shape",d,s,p) for p in ("zero-barrier","curvature-horizon") for d,s in (([41,31,1],[.7,1.,1.]),([41,29,23],[.7,1.,1.3]))])
    pathlib.Path(__file__).with_name('itk-5.4.json').write_text(json.dumps(document,separators=(',',':'),allow_nan=False)+'\n')
if __name__=='__main__':main()
