package com.enterpriseim.client.tests;

import android.app.*;
import android.content.Intent;
import android.os.Bundle;
import org.json.JSONObject;
import java.io.*;

/** Public Activity.recreate and actual lifecycle callbacks; no production hooks. */
public final class HandoffLifecycleInstrumentation extends Instrumentation {
    private volatile Activity activity;
    private volatile int created,destroyed;
    private String owner;
    @Override public void onCreate(Bundle arguments){
        super.onCreate(arguments);owner=arguments.getString("owner","");
        ((Application)getTargetContext().getApplicationContext()).registerActivityLifecycleCallbacks(new Application.ActivityLifecycleCallbacks(){
            public void onActivityCreated(Activity value,Bundle state){if(value.getClass().getName().equals("com.enterpriseim.client.MainActivity")){activity=value;created++;}}
            public void onActivityDestroyed(Activity value){if(value.getClass().getName().equals("com.enterpriseim.client.MainActivity"))destroyed++;}
            public void onActivityStarted(Activity a){}public void onActivityResumed(Activity a){}public void onActivityPaused(Activity a){}public void onActivityStopped(Activity a){}public void onActivitySaveInstanceState(Activity a,Bundle state){}
        });start();
    }
    private void waitFile(File file)throws Exception{long deadline=android.os.SystemClock.elapsedRealtime()+150000;while(!file.exists()&&android.os.SystemClock.elapsedRealtime()<deadline)Thread.sleep(30);if(!file.exists())throw new IOException("Lifecycle control deadline");}
    @Override public void onStart(){Bundle result=new Bundle();try{
        if(!owner.matches("[a-f0-9]{32}"))throw new IllegalArgumentException("owner");
        File prefix=new File(getTargetContext().getCacheDir(),"im-handoff-"+owner);waitFile(new File(prefix+"-recreate"));
        Activity original=activity;if(original==null||original.isFinishing())throw new IllegalStateException("Actual Activity unavailable");
        int originalId=System.identityHashCode(original),beforeCreated=created,beforeDestroyed=destroyed;
        runOnMainSync(original::recreate);
        long deadline=android.os.SystemClock.elapsedRealtime()+20000;
        while((created<=beforeCreated||destroyed<=beforeDestroyed||activity==original)&&android.os.SystemClock.elapsedRealtime()<deadline)Thread.sleep(30);
        if(created<=beforeCreated||destroyed<=beforeDestroyed||activity==original)throw new AssertionError("Recreation not observed");
        JSONObject json=new JSONObject();json.put("pid",android.os.Process.myPid()).put("original_identity",originalId).put("new_identity",System.identityHashCode(activity)).put("created",created).put("destroyed",destroyed).put("actual_activity_class",activity.getClass().getName());
        try(FileOutputStream out=new FileOutputStream(new File(prefix+"-recreated.json"))){out.write(json.toString().getBytes(java.nio.charset.StandardCharsets.UTF_8));out.getFD().sync();}
        waitFile(new File(prefix+"-finish"));result.putString("stream","ACTIVITY_HANDOFF_PASSED "+json);finish(-1,result);
    }catch(Throwable error){result.putString("stream",android.util.Log.getStackTraceString(error));result.putInt("failed",1);finish(0,result);}}
}
