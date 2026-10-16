package com.enterpriseim.client.tests;

import android.app.Instrumentation;
import android.os.Bundle;
import android.net.Uri;
import com.enterpriseim.client.ExportJournal;
import com.enterpriseim.client.ExportLedger;
import java.io.*;
import java.nio.file.Files;
import java.util.*;

/** Actual target-APK state machine and Android AtomicFile; fault only resource boundaries. */
public final class RecoveryInstrumentation extends Instrumentation {
    private String owner;
    private final List<String> passed = new ArrayList<>();
    @Override public void onCreate(Bundle args) { super.onCreate(args); owner=args.getString("owner", "");start(); }
    private static void check(boolean ok) { if (!ok) throw new AssertionError(); }
    private interface Action { void run() throws Exception; }
    private static void fails(Action action) throws Exception { boolean failed=false;try { action.run(); }catch(Exception e){failed=true;}check(failed); }
    private static final Uri URI=Uri.parse("content://controlled.export/document/owned");
    private static final String TICKET="40904c27-330c-4187-948f-b608d372d980";
    private static final class Filesystem implements ExportLedger.Resources {
        final File document; int held=0, deletes=0; boolean block=false, throwsDelete=false, releaseFailure=false, takeFailure=false, ambiguous=false;
        Filesystem(File root) throws Exception { document=new File(root,"partial");Files.write(document.toPath(),new byte[]{5,8,13}); }
        public int grants(Uri uri){check(uri.equals(URI));return held;}
        public void take(Uri uri,int flags)throws Exception{if(takeFailure)throw new IOException();held|=flags;}
        public void release(Uri uri,int flags)throws Exception{if(releaseFailure)throw new IOException();held&=~flags;}
        public boolean delete(Uri uri)throws Exception{check(uri.equals(URI));deletes++;if(throwsDelete)throw new IOException();if(block)return false;return document.delete();}
        public boolean absent(Uri uri)throws Exception{if(ambiguous)throw new IOException();return !document.exists();}
    }
    private File root(String name)throws Exception{File file=new File(getTargetContext().getNoBackupFilesDir(),"recovery-tests-"+owner+"-"+name);check(file.mkdir());return file;}
    private void remove(File file)throws Exception{if(file.isDirectory())for(File child:file.listFiles())remove(child);check(file.delete());}
    @Override public void onStart(){Bundle result=new Bundle();try {
        check(owner.matches("[a-f0-9]{32}"));
        for(String name:new String[]{"reload","delete-false","delete-throws","committed","deleted-receipt","release-fails","preexisting-grant","take-fails","already-absent","ambiguous-provider","atomic-interruption","corrupt","unknown-version","oversized","orphan-new"}){
            File dir=root(name);ExportJournal journal=new ExportJournal(dir);Filesystem resource=new Filesystem(dir);ExportLedger ledger=new ExportLedger(journal,resource);
            if(name.equals("corrupt")||name.equals("unknown-version")||name.equals("oversized")||name.equals("orphan-new")){
                byte[] bytes=name.equals("oversized")?new byte[17000]:name.equals("unknown-version")?"{\"version\":99,\"ticket\":\"40904c27-330c-4187-948f-b608d372d980\",\"uri\":\"content://controlled.export/document/owned\",\"acquired\":3,\"phase\":\"PENDING\"}".getBytes():"partial-json".getBytes();
                File metadata=new File(dir,"export-cleanup.json"+(name.equals("orphan-new")?".new":""));Files.write(metadata.toPath(),bytes);fails(ledger::recover);check(resource.document.exists()&&resource.deletes==0&&metadata.exists());
            }else{
                if(name.equals("preexisting-grant"))resource.held=1;
                if(name.equals("take-fails")){resource.takeFailure=true;resource.block=true;fails(()->ledger.prepare(TICKET,URI,67));check(journal.read().phase.equals("PREPARED"));fails(ledger::recover);check(resource.document.exists());resource.block=false;ledger.recover();check(!resource.document.exists());}
                else{
                    ledger.prepare(TICKET,URI,67);check(journal.read().phase.equals("PENDING"));
                    if(name.equals("delete-false")||name.equals("delete-throws")){resource.block=true;resource.throwsDelete=name.equals("delete-throws");fails(ledger::recover);check(resource.document.exists()&&journal.read().phase.equals("PENDING"));resource.block=false;resource.throwsDelete=false;}
                    if(name.equals("committed"))ledger.commit(TICKET);
                    if(name.equals("deleted-receipt"))journal.write(new ExportJournal.Entry(TICKET,URI,3,"DELETED"));
                    if(name.equals("release-fails")){resource.releaseFailure=true;fails(ledger::recover);check(!resource.document.exists()&&journal.read().phase.equals("DELETED"));resource.releaseFailure=false;}
                    if(name.equals("already-absent"))check(resource.document.delete());
                    if(name.equals("ambiguous-provider")){resource.throwsDelete=true;resource.ambiguous=true;fails(ledger::recover);check(resource.document.exists()&&journal.read()!=null);resource.throwsDelete=false;resource.ambiguous=false;}
                    if(name.equals("atomic-interruption"))Files.write(new File(dir,"export-cleanup.json.new").toPath(),"incomplete replacement".getBytes());
                    // A new object reloads the real on-disk metadata.
                    new ExportLedger(new ExportJournal(dir),resource).recover();check(journal.read()==null);
                    if(name.equals("committed")||name.equals("deleted-receipt")){check(resource.document.exists()&&resource.deletes==0);check(Arrays.equals(Files.readAllBytes(resource.document.toPath()),new byte[]{5,8,13}));}
                    else check(!resource.document.exists());
                    check(resource.held==(name.equals("preexisting-grant")?1:0));
                }
            }
            remove(dir);passed.add(name);
        }
        int treePassed=TreeAllocationChecks.run(new File(getTargetContext().getNoBackupFilesDir(),"tree-allocation-"+owner));
        result.putString("stream","TREE_ALLOCATION_ALL_PASSED "+treePassed+"\nRECOVERY_ALL_PASSED "+passed.size()+" "+passed+" target="+getTargetContext().getApplicationInfo().sourceDir+" classloader="+ExportLedger.class.getClassLoader());result.putInt("passed",passed.size());finish(-1,result);
    }catch(Throwable error){result.putString("stream",android.util.Log.getStackTraceString(error)+" passed="+passed);result.putInt("failed",1);finish(0,result);}}
}
