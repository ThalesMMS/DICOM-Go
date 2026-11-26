import json,hashlib
from pathlib import Path
import SimpleITK as sitk
import numpy as np
assert sitk.Version_VersionString()=='2.5.3' and sitk.Version_ITKVersionString().startswith('5.4'), 'pinned oracle required'
out=Path(__file__).resolve().parent
out.mkdir(parents=True,exist_ok=True)
cases=[]
for dimension in (2,3):
 for trial in range(10):
  shape=(7,9) if dimension==2 else (5,7,9)
  a=np.zeros(shape,np.uint8)
  if trial==0:
   a[(slice(1,-1),)*dimension]=1
  elif trial==1:
   a[:]=1; a[(0,)*dimension]=0
  elif trial==2:
   a[(2,)*dimension]=1
  elif trial==3:
   a[(slice(0,3),)*dimension]=1; a[(slice(-3,None),)*dimension]=1
  elif trial==4:
   for index in np.ndindex(shape):
    a[index]=int(sum(index)%2==0)
  else:
   # Explicit arithmetic pattern: deterministic and independent of RNG versions.
   for index in np.ndindex(shape):
    x=index[-1]; y=index[-2]; z=index[0] if dimension==3 else 0
    a[index]=int((x*17+y*11+z*7+trial*13)%19 < 4+trial)
  spacing=(.5,2.) if trial%2==0 else (.7,1.3)
  if dimension==3: spacing=spacing+(2.5,)
  image=sitk.GetImageFromArray(a); image.SetSpacing(spacing)
  results=[]
  for positive in (False,True):
   for squared in (False,True):
    value=sitk.GetArrayFromImage(sitk.SignedMaurerDistanceMap(image,insideIsPositive=positive,squaredDistance=squared,useImageSpacing=True))
    assert np.isfinite(value).all() and np.abs(value).max()<1000
    results.append({'InsideIsPositive':positive,'SquaredDistance':squared,'Values':value.ravel().tolist()})
  cases.append({'Name':f'{dimension}d-{trial}','Dimensions':[9,7,5 if dimension==3 else 1],'Spacing':list(spacing)+( [1.] if dimension==2 else [] ),'Foreground':np.flatnonzero(a.ravel()).tolist(),'Results':results})
fixture={'Generator':'SimpleITK 2.5.3 / ITK5.4 SignedMaurerDistanceMap: UseImageSpacing=true, zero background, float32 output; generated synthetic inputs','Tolerance':'absolute1e-5 + relative2e-6, set before production comparison','Cases':cases}
data=(json.dumps(fixture,indent=2)+'\n').encode()
(out/'itk-5.4.json').write_bytes(data)
print(json.dumps({'cases':len(cases),'results':sum(len(c['Results']) for c in cases),'values':sum(len(r['Values']) for c in cases for r in c['Results']),'sha256':hashlib.sha256(data).hexdigest(),'version':str(sitk.Version()),'bytes':len(data)}))
