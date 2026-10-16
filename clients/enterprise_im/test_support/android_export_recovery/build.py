"""Build recovery instrumentation linked to production classes in the installed target APK."""
from pathlib import Path
import argparse,os,subprocess,zipfile
p=argparse.ArgumentParser();p.add_argument('--sdk',type=Path,required=True);p.add_argument('--java-home',type=Path,required=True);p.add_argument('--target-classes',type=Path,required=True);p.add_argument('--debug-keystore',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args();a.out.mkdir(parents=True,exist_ok=True);source=Path(__file__).resolve().parent;tools=a.sdk/'build-tools/36.0.0';android=a.sdk/'platforms/android-36/android.jar';env=dict(os.environ,JAVA_HOME=str(a.java_home));env['PATH']=str(a.java_home/'bin')+':'+env.get('PATH','')
def run(args):subprocess.run([str(n) for n in args],env=env,check=True)
classes=a.out/'classes';classes.mkdir(exist_ok=True);dex=a.out/'dex';dex.mkdir(exist_ok=True)
run([a.java_home/'bin/javac','-source','17','-target','17','-classpath',str(android)+os.pathsep+str(a.target_classes),'-d',classes,source/'RecoveryInstrumentation.java',source/'TreeAllocationChecks.java'])
assert not list(classes.rglob('ExportJournal.class')) and not list(classes.rglob('ExportLedger.class')) and not list(classes.rglob('ExportDirectory.class'))
run([tools/'d8','--lib',android,'--classpath',a.target_classes,'--min-api','26','--output',dex,*sorted(classes.rglob('*.class'))])
unsigned=a.out/'unsigned.apk';run([tools/'aapt2','link','--manifest',source/'AndroidManifest.xml','-I',android,'-o',unsigned])
with zipfile.ZipFile(unsigned,'a') as z:z.write(dex/'classes.dex','classes.dex')
aligned=a.out/'aligned.apk';run([tools/'zipalign','-f','4',unsigned,aligned]);run([tools/'apksigner','sign','--ks',a.debug_keystore,'--ks-key-alias','androiddebugkey','--ks-pass','pass:android','--key-pass','pass:android','--out',a.out/'recovery-tests.apk',aligned]);run([tools/'apksigner','verify','--verbose',a.out/'recovery-tests.apk'])
