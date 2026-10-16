package com.enterpriseim.client.tests;

import android.app.*;
import android.os.Bundle;
import com.enterpriseim.client.ExportManager;
import org.json.JSONObject;
import java.io.*;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/** Holds the real serial worker so a returned directory result is queued across Activity.recreate. */
public final class ReauthorizationLifecycleInstrumentation extends Instrumentation {
    private volatile Activity activity;
    private volatile int created, destroyed;
    private String owner;
    private final CountDownLatch release = new CountDownLatch(1);
    @Override public void onCreate(Bundle arguments) {
        super.onCreate(arguments); owner=arguments.getString("owner", "");
        ((Application)getTargetContext().getApplicationContext()).registerActivityLifecycleCallbacks(new Application.ActivityLifecycleCallbacks() {
            public void onActivityCreated(Activity a,Bundle state){if(a.getClass().getName().equals("com.enterpriseim.client.MainActivity")){activity=a;created++;}}
            public void onActivityDestroyed(Activity a){if(a.getClass().getName().equals("com.enterpriseim.client.MainActivity"))destroyed++;}
            public void onActivityStarted(Activity a){}public void onActivityResumed(Activity a){}public void onActivityPaused(Activity a){}public void onActivityStopped(Activity a){}public void onActivitySaveInstanceState(Activity a,Bundle state){}
        }); start();
    }
    private void waitFile(File f)throws Exception{long end=android.os.SystemClock.elapsedRealtime()+180000;while(!f.exists()&&android.os.SystemClock.elapsedRealtime()<end)Thread.sleep(20);if(!f.exists())throw new IOException("lifecycle control deadline");}
    private void save(File f,JSONObject value)throws Exception{try(FileOutputStream out=new FileOutputStream(f)){out.write(value.toString().getBytes(java.nio.charset.StandardCharsets.UTF_8));out.getFD().sync();}}
    @Override public void onStart() {
        Bundle result=new Bundle();
        try {
            if(!owner.matches("[a-f0-9]{32}"))throw new IllegalArgumentException("owner");
            File prefix=new File(getTargetContext().getCacheDir(),"im-reauth-life-"+owner);waitFile(new File(prefix+"-hold"));
            CountDownLatch entered=new CountDownLatch(1),drained=new CountDownLatch(1);
            ExportManager manager=ExportManager.get(getTargetContext());
            manager.worker.execute(()->{entered.countDown();try{if(!release.await(180,TimeUnit.SECONDS))throw new IllegalStateException("worker gate deadline");}catch(InterruptedException e){Thread.currentThread().interrupt();}});
            if(!entered.await(10,TimeUnit.SECONDS))throw new AssertionError("worker not held");
            save(new File(prefix+"-held.json"),new JSONObject().put("pid",android.os.Process.myPid()).put("worker_held",true));
            waitFile(new File(prefix+"-recreate"));Activity original=activity;
            if(original==null||original.isFinishing())throw new AssertionError("actual Activity missing");
            int identity=System.identityHashCode(original),oldCreated=created,oldDestroyed=destroyed;runOnMainSync(original::recreate);
            long end=android.os.SystemClock.elapsedRealtime()+20000;while((created<=oldCreated||destroyed<=oldDestroyed||activity==original)&&android.os.SystemClock.elapsedRealtime()<end)Thread.sleep(20);
            if(created<=oldCreated||destroyed<=oldDestroyed||activity==original)throw new AssertionError("actual recreation not observed");
            JSONObject proof=new JSONObject().put("pid",android.os.Process.myPid()).put("original_identity",identity).put("new_identity",System.identityHashCode(activity)).put("created",created).put("destroyed",destroyed);
            save(new File(prefix+"-recreated.json"),proof);waitFile(new File(prefix+"-release"));release.countDown();manager.worker.execute(drained::countDown);
            if(!drained.await(20,TimeUnit.SECONDS))throw new AssertionError("real worker not drained");
            save(new File(prefix+"-drained.json"),proof.put("worker_drained",true));waitFile(new File(prefix+"-finish"));
            result.putString("stream","REAUTHORIZATION_LIFECYCLE_PASSED "+proof);finish(-1,result);
        } catch(Throwable error){result.putString("stream",android.util.Log.getStackTraceString(error));result.putInt("failed",1);finish(0,result);}
        finally {release.countDown();}
    }
}
