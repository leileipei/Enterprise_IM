package com.enterpriseim.client.tests;

import android.app.Instrumentation;
import android.os.Bundle;
import com.enterpriseim.client.ExportSession;
import java.io.File;

public final class ExportSessionInstrumentation extends Instrumentation {
    private String owner;
    @Override public void onCreate(Bundle arguments) { super.onCreate(arguments);owner=arguments.getString("owner","");start(); }
    @Override public void onStart() {
        Bundle result=new Bundle();
        try {
            if(!owner.matches("[a-f0-9]{32}"))throw new IllegalArgumentException("fresh owner required");
            int passed=ExportSessionChecks.run(new File(getTargetContext().getCacheDir(),"im-session-"+owner));
            ReliableCloseChecks.run(new File(getTargetContext().getCacheDir(),"im-reliable-"+owner));passed++;
            result.putString("stream","SESSION_ALL_PASSED "+passed+" target="+getTargetContext().getApplicationInfo().sourceDir+" classloader="+ExportSession.class.getClassLoader());
            result.putInt("passed",passed);finish(-1,result);
        }catch(Throwable failure){result.putString("stream",android.util.Log.getStackTraceString(failure));result.putInt("failed",1);finish(0,result);}
    }
}
