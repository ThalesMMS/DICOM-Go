#!/usr/bin/env python3
"""Own synthetic fixtures, generated only through pinned public SimpleITK APIs.
No implementation/header source is accessed. Outputs contain no patient data.
"""
import argparse, base64, hashlib, json, math, platform, tempfile
from pathlib import Path
import SimpleITK as sitk

PIN_SITK = "2.5.3"
PIN_ITK = "5.4"

def versions():
    actual = {"simpleitk": sitk.Version_VersionString(), "itk": sitk.Version_ITKVersionString(), "python": platform.python_version()}
    if actual["simpleitk"] != PIN_SITK or actual["itk"] != PIN_ITK:
        raise RuntimeError(f"public oracle version mismatch: {actual}")
    return actual

def descriptor(t):
    d = {"class": t.GetName(), "dimension": t.GetDimension(), "parameters": list(t.GetParameters()), "fixed": list(t.GetFixedParameters())}
    if hasattr(t, "GetMatrix"):
        d["matrix"] = list(t.GetMatrix())
    if isinstance(t, sitk.CompositeTransform):
        d["components"] = [descriptor(t.GetNthTransform(i)) for i in range(t.GetNumberOfTransforms())]
    return d

def physical(index, origin, spacing, direction):
    return tuple(origin[r] + sum(direction[3*r+c]*spacing[c]*index[c] for c in range(3)) for r in range(3))

def cases():
    out = []
    points3 = [(0,0,0),(2,-3,4),(1.5,2.25,-3.5),(-12,7,20)]
    points2 = [(0,0),(2,-3),(1.5,2.25),(-12,7)]
    def add(name, t, points=None, dense=False):
        out.append((name,t,points or (points3 if t.GetDimension()==3 else points2),dense))
    tr3 = sitk.TranslationTransform(3);tr3.SetParameters([1.25,-2.5,3.75]);add("translation3",tr3)
    sv = sitk.ScaleVersor3DTransform();sv.SetFixedParameters([2,-3,4]);sv.SetParameters([.1,.2,.3,1.5,-2,3,1.25,.8,1.1]);add("scaleversor3",sv)
    sk = sitk.ScaleSkewVersor3DTransform();sk.SetFixedParameters([2,-3,4]);sk.SetParameters([.1,.2,.3,1.5,-2,3,1.25,.8,1.1,.125,-.25,.375,.5,-.625,.75]);add("scaleskewversor3",sk)
    for name,ctor,params in [("scaleversor_zero",sitk.ScaleVersor3DTransform,[0,0,0,1,-2,3,0,1,1]),("scaleversor_negative",sitk.ScaleVersor3DTransform,[0,0,0,1,-2,3,-1,1,1]),("scaleskew_negative",sitk.ScaleSkewVersor3DTransform,[0,0,0,1,-2,3,-1,1,1,.125,-.25,.375,.5,-.625,.75]),("scaleversor_clamp",sitk.ScaleVersor3DTransform,[2,0,0,1,-2,3,1,1,1])]:
        t=ctor();t.SetParameters(params);add(name,t)
    e = sitk.Euler3DTransform();e.SetCenter([2,-3,4]);e.SetParameters([.2,-.3,.4,1.5,-2,3]);add("euler3_zxy",e)
    ez = sitk.Euler3DTransform(e);ez.SetComputeZYX(True);add("euler3_zyx",ez)
    v = sitk.VersorRigid3DTransform();v.SetFixedParameters([2,-3,4]);v.SetParameters([.1,.2,.3,1.5,-2,3]);add("versorrigid3",v)
    sim = sitk.Similarity3DTransform();sim.SetFixedParameters([2,-3,4]);sim.SetParameters([.1,.2,.3,1.5,-2,3,1.2]);add("similarity3",sim)
    a = sitk.AffineTransform(3);a.SetFixedParameters([2,-3,4]);a.SetParameters([1.1,.2,-.1,-.3,.9,.15,.05,-.2,1.2,1.5,-2,3]);add("affine3",a)
    tr2 = sitk.TranslationTransform(2);tr2.SetParameters([1.25,-2.5]);add("translation2",tr2)
    e2 = sitk.Euler2DTransform();e2.SetFixedParameters([2,-3]);e2.SetParameters([.4,1.5,-2]);add("euler2",e2)
    s2 = sitk.Similarity2DTransform();s2.SetFixedParameters([2,-3]);s2.SetParameters([1.25,.4,1.5,-2]);add("similarity2",s2)
    for scale,name in [(0,"similarity2_zero"),(-1,"similarity2_negative")]:
        t=sitk.Similarity2DTransform(s2);t.SetScale(scale);add(name,t)
    a2 = sitk.AffineTransform(2);a2.SetFixedParameters([2,-3]);a2.SetParameters([1.1,.2,-.3,.9,1.5,-2]);add("affine2",a2)
    c2 = sitk.CompositeTransform(2);c2.AddTransform(tr2);c2.AddTransform(a2);c2.AddTransform(e2);add("composite2",c2)
    c3 = sitk.CompositeTransform(3);c3.AddTransform(tr3);c3.AddTransform(sk);c3.AddTransform(e);add("composite3",c3)
    co,si = math.cos(.3),math.sin(.3)
    direction=[co,-si,0,si,co,0,0,0,1]
    origin=[-3,2,5];spacing=[1.2,1.7,.8]
    b = sitk.BSplineTransform(3,3);b.SetFixedParameters([5,5,4]+origin+spacing+direction)
    b.SetParameters([((13*i)%29-14)/32 for i in range(b.GetNumberOfParameters())])
    bp=[physical(x,origin,spacing,direction) for x in [(1.2,1.3,1.1),(2.2,2.1,1.8),(1,1,1),(3,3,2),(.5,1,1),(4,2,1),(-1,0,0)]]
    add("bspline3",b,bp)
    image = sitk.Image([5,4,3],sitk.sitkVectorFloat64,3);image.SetOrigin(origin);image.SetSpacing(spacing);image.SetDirection(direction)
    for z in range(3):
        for y in range(4):
            for x in range(5):
                i=(z*4+y)*5+x
                image.SetPixel([x,y,z],tuple(((37*(3*i+c))%101-50)/64 for c in range(3)))
    field=sitk.DisplacementFieldTransform(image)
    fp=[physical(x,origin,spacing,direction) for x in [(0,0,0),(1.3,2.1,1.1),(4,3,2),(-.49,0,0),(-.51,0,0),(4.49,3,2),(4.51,3,2)]]
    add("field3",field,fp)
    cf=sitk.CompositeTransform(3);cf.AddTransform(a);cf.AddTransform(field);add("composite_affine_field3",cf,fp)
    cb=sitk.CompositeTransform(3);cb.AddTransform(tr3);cb.AddTransform(b);add("composite_translation_bspline3",cb,bp)
    many=sitk.CompositeTransform(3)
    for i in range(9):
        t=sitk.TranslationTransform(3);t.SetParameters([(i+1)/8,-(i+1)/16,(i+1)/32]);many.AddTransform(t)
    add("many_dense3",many,points3,True)
    return out

def generate(output):
    version=versions();results=[]
    with tempfile.TemporaryDirectory(prefix="itk1345-public-") as work:
        for name,t,pts,dense in cases():
            expected=descriptor(t);record={"name":name,**expected,"points":[{"input":list(p),"output":list(t.TransformPoint(p))} for p in pts],"files":{},"hdf5_dense_links":dense}
            formats=["tfm","h5"]
            if not isinstance(t,(sitk.CompositeTransform,sitk.BSplineTransform,sitk.DisplacementFieldTransform)):
                formats.append("mat")
            for fmt in formats:
                path=Path(work)/(name+"."+fmt);sitk.WriteTransform(t,str(path));actual=sitk.ReadTransform(str(path))
                if actual.GetDimension()!=t.GetDimension():raise AssertionError("dimension changed")
                for p in record["points"]:
                    got=actual.TransformPoint(p["input"])
                    if max(abs(a-b) for a,b in zip(got,p["output"]))>1e-12:raise AssertionError("public self-roundtrip changed point")
                data=path.read_bytes();record["files"][fmt]={"base64":base64.b64encode(data).decode("ascii"),"bytes":len(data),"sha256":hashlib.sha256(data).hexdigest()}
            results.append(record)
    doc={"provenance":{"method":"own synthetic public SimpleITK API only","versions":version,"base":"6dea777b047a65508d5fe7a2c8627ef01a58f515","not_implementation_source":True},"cases":results}
    encoded=json.dumps(doc,indent=2,allow_nan=False).encode()+b"\n"
    if len(encoded)>8*1024*1024:raise RuntimeError("fixture admission exceeded8MiB")
    output.parent.mkdir(parents=True,exist_ok=True);output.write_bytes(encoded)
    print(json.dumps({"output":str(output),"cases":len(results),"files":sum(len(x["files"]) for x in results),"sha256":hashlib.sha256(encoded).hexdigest(),"versions":version},sort_keys=True))
if __name__=="__main__":
    parser=argparse.ArgumentParser();parser.add_argument("--output",type=Path,default=Path(__file__).with_name("fixtures.json"));args=parser.parse_args();generate(args.output)
