#!/usr/bin/env python3
"""Real public ITK reads of Go outputs, independent metadata inspection and rewrites.
Expectations come from frozen public fixtures, never from a Go output echo.
"""
import argparse, hashlib, json, math, platform
from pathlib import Path
import h5py
import SimpleITK as sitk

def close_vector(label, actual, expected):
    if len(actual) != len(expected): raise AssertionError(f"{label} length mismatch")
    for i,(a,b) in enumerate(zip(actual,expected)):
        if not math.isfinite(a) or abs(a-b)>2e-11*max(1,abs(b)):
            raise AssertionError(f"{label}[{i}] {a!r} != {b!r}")

def check_metadata(t, record):
    if t.GetDimension()!=record["dimension"]: raise AssertionError("dimension changed")
    if record.get("components"):
        composite=sitk.CompositeTransform(t)
        if composite.GetNumberOfTransforms()!=len(record["components"]): raise AssertionError("component count")
        for i,component in enumerate(record["components"]):
            check_metadata(composite.GetNthTransform(i),component)
    else:
        close_vector("parameters",t.GetParameters(),record["parameters"])
        close_vector("fixed",t.GetFixedParameters(),record["fixed"])
        if "matrix" in record:
            concrete=getattr(sitk,record["class"])(t)
            close_vector("matrix",concrete.GetMatrix(),record["matrix"])

def check_points(t, record):
    check_metadata(t,record)
    for i,p in enumerate(record["points"]):close_vector(f"point{i}",t.TransformPoint(p["input"]),p["output"])

def inspect_hdf5(path):
    datasets=[]
    with h5py.File(path,"r") as file:
        def inspect(name,obj):
            if not isinstance(obj,h5py.Dataset):return
            string=h5py.check_string_dtype(obj.dtype)
            if string is not None:
                if string.encoding!="ascii" or string.length is not None or obj.id.get_type().get_cset()!=0:
                    raise AssertionError(f"{name}: expected ASCII VLEN public ITK schema")
                value=obj[...].tolist()
                if len(value)!=1 or len(value[0])>1024:raise AssertionError("bounded scalar string shape")
                value[0].decode("ascii")
                datasets.append({"path":name,"kind":"ASCII VLEN","bytes":len(value[0]),"charset":0})
            else:
                if obj.dtype.kind!="f" or obj.dtype.itemsize!=8 or len(obj.shape)!=1:raise AssertionError("float64 rank1 layout")
                if not all(math.isfinite(float(x)) for x in obj[...]):raise AssertionError("nonfinite vector")
                datasets.append({"path":name,"kind":"float64","count":obj.shape[0]})
        file.visititems(inspect)
    return datasets

def verify(args):
    versions={"simpleitk":sitk.Version_VersionString(),"itk":sitk.Version_ITKVersionString(),"h5py":h5py.__version__,"hdf5":h5py.version.hdf5_version,"python":platform.python_version()}
    if versions["simpleitk"]!="2.5.3" or versions["itk"]!="5.4":raise RuntimeError(f"oracle version mismatch: {versions}")
    fixtures=json.loads(args.fixtures.read_text());cases={c["name"]:c for c in fixtures["cases"]}
    manifest=json.loads((args.go_output/"manifest.json").read_text())
    if len(manifest)!=68:raise AssertionError("export campaign file count")
    args.rewrite.mkdir(parents=True,exist_ok=True)
    records=[]
    for m in manifest:
        source=args.go_output/m["file"];data=source.read_bytes()
        if len(data)!=m["bytes"] or hashlib.sha256(data).hexdigest()!=m["sha256"]:raise AssertionError("Go output byte provenance")
        case=cases[m["name"]]
        transform=sitk.ReadTransform(str(source));check_points(transform,case)
        inspected=inspect_hdf5(source) if m["format"]=="h5" else None
        output=args.rewrite/m["file"];sitk.WriteTransform(transform,str(output));check_points(sitk.ReadTransform(str(output)),case)
        rewritten=output.read_bytes()
        records.append({**m,"public_reader":"PASS","rewritten_sha256":hashlib.sha256(rewritten).hexdigest(),"rewritten_bytes":len(rewritten),"hdf5_public_inspection":inspected})
    extras=[]
    if args.extra_hdf:
        hierarchy=args.extra_hdf/"classic-empty-large.h5"
        inspected=inspect_hdf5(hierarchy)
        with h5py.File(hierarchy,"r") as f:
            if f["EmptyString"][0]!=b"" or f["EmptyVector"].shape!=(0,) or len(f["Group"])!=1024:raise AssertionError("empty/hierarchy metadata")
            for boundary in ["0000","0255","0256","1023"]:
                if f["Group"][boundary]["Value"][0]!=3.25:raise AssertionError("multi-level classic key boundary")
        extras.append({"file":hierarchy.name,"sha256":hashlib.sha256(hierarchy.read_bytes()).hexdigest(),"public_hdf5_inspection":"PASS","datasets":len(inspected)})
        maximum=args.extra_hdf/"maximum-records.h5"
        inspected=inspect_hdf5(maximum)
        t=sitk.CompositeTransform(sitk.ReadTransform(str(maximum)))
        if t.GetNumberOfTransforms()!=1023:raise AssertionError("maximum transform records")
        close_vector("maximum-records-point",t.TransformPoint([0,0,0]),[1023*.125,-1023*.0625,1023*.03125])
        extras.append({"file":maximum.name,"sha256":hashlib.sha256(maximum.read_bytes()).hexdigest(),"public_hdf5_inspection":"PASS","public_itk_reader":"PASS","datasets":len(inspected),"components":1023})
    report={"versions":versions,"fixtures_sha256":hashlib.sha256(args.fixtures.read_bytes()).hexdigest(),"not_implementation_source":True,"files":records,"extra_hdf5":extras,"counts":{"cases":len(cases),"go_to_itk":len(records),"itk_rewrites":len(records)}}
    args.report.write_text(json.dumps(report,indent=2,allow_nan=False)+"\n")
    print(json.dumps({"versions":versions,"counts":report["counts"],"report_sha256":hashlib.sha256(args.report.read_bytes()).hexdigest()},sort_keys=True))

if __name__=="__main__":
    p=argparse.ArgumentParser();p.add_argument("--go-output",type=Path,required=True);p.add_argument("--rewrite",type=Path,required=True);p.add_argument("--report",type=Path,required=True);p.add_argument("--extra-hdf",type=Path);p.add_argument("--fixtures",type=Path,default=Path(__file__).with_name("fixtures.json"));verify(p.parse_args())
