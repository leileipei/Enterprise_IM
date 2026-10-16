package com.enterpriseim.client.tests;

import android.content.Context;
import android.content.ContextWrapper;
import android.net.Uri;
import com.enterpriseim.client.*;
import java.io.*;
import java.lang.reflect.*;
import java.util.concurrent.atomic.AtomicBoolean;

/** Target-APK manager, real journal/file effects, controlled permission boundary. */
final class ManagerRecoveryChecks {
    private static final Uri TREE=Uri.parse("content://controlled.export/tree/root");
    private static final Uri DOC=Uri.parse("content://controlled.export/tree/root/document/owned");
    private static final String TICKET="c6b69004-62d7-4ca7-9e92-14f51fedb0be";
    private static void check(boolean ok,String message){if(!ok)throw new AssertionError(message);}
    private static final class Resources implements ExportLedger.Resources {
        final File document;int held,deletes,takes;Runnable duringTake=()->{};
        Resources(File dir){document=new File(dir,"owned.bin");}
        public int grants(Uri tree){return held;}
        public void take(Uri tree,int flags){takes++;held|=flags;duringTake.run();}
        public void release(Uri tree,int flags){held&=~flags;}
        public void validateTree(Uri tree){}
        public void create(Uri tree,String name)throws Exception{try(FileOutputStream out=new FileOutputStream(document)){out.write(new byte[]{5,8,13});}}
        public Uri findAllocated(Uri tree,String prefix){if(held!=3)throw new SecurityException();return document.exists()?DOC:null;}
        public boolean delete(Uri uri){if(held!=3)throw new SecurityException();deletes++;return document.delete();}
        public boolean absent(Uri uri){return !document.exists();}
    }
    private static void field(Object object,String name,Object value)throws Exception{Field f=object.getClass().getDeclaredField(name);f.setAccessible(true);f.set(object,value);}
    private static byte[] bytes(File file)throws Exception{try(FileInputStream in=new FileInputStream(file);ByteArrayOutputStream out=new ByteArrayOutputStream()){byte[] buffer=new byte[1024];int n;while((n=in.read(buffer))!=-1)out.write(buffer,0,n);return out.toByteArray();}}
    private static void remove(File file){if(file.isDirectory())for(File child:file.listFiles())remove(child);check(file.delete(),"fixture teardown");}
    static int run(Context context,File parent)throws Exception{
        check(parent.mkdir(),"fresh manager fixture");int passed=0;
        for(String mode:new String[]{"ordinary-destroy","closed-before-take","destroy-during-take"}){
            File dir=new File(parent,mode);check(dir.mkdir(),"fresh case");File metadata=new File(dir,"metadata");check(metadata.mkdir(),"fresh journal");
            ExportJournal journal=new ExportJournal(metadata);Resources resources=new Resources(dir);ExportLedger ledger=new ExportLedger(journal,resources);ledger.allocateTree(TICKET,TREE,"report.bin",67);
            Constructor<ExportManager> constructor=ExportManager.class.getDeclaredConstructor(Context.class);constructor.setAccessible(true);
            ExportManager manager=constructor.newInstance(new ContextWrapper(context){@Override public File getNoBackupFilesDir(){return metadata;}});field(manager,"ledger",ledger);
            Method recover=ExportManager.class.getDeclaredMethod("recover");recover.setAccessible(true);
            ExportSession session=new ExportSession(()->new ByteArrayOutputStream(),()->{try{recover.invoke(manager);}catch(InvocationTargetException error){throw (Exception)error.getCause();}return true;});session.write(new byte[]{21,34});
            Class<?> type=Class.forName("com.enterpriseim.client.ExportManager$Active");Constructor<?> active=type.getDeclaredConstructor(String.class,String.class,ExportSession.class);active.setAccessible(true);field(manager,"active",active.newInstance("owner",TICKET,session));
            try{
                if(mode.equals("ordinary-destroy")){
                    manager.destroy("owner");check(!resources.document.exists()&&journal.read()==null&&resources.deletes==1&&resources.held==0,"ordinary active export must still clean on destruction");
                }else{
                    resources.held=0;try{manager.abort("owner",TICKET);throw new AssertionError("abort must fail with lost permission");}catch(ExportManager.CleanupException expected){}
                    ExportLedger.Reauthorization request;try{manager.reserve("owner");throw new AssertionError("reserve must request original directory");}catch(ExportManager.ReauthorizationException expected){request=expected.request;}
                    byte[] original=bytes(new File(metadata,"export-cleanup.json"));AtomicBoolean open=new AtomicBoolean(!mode.equals("closed-before-take"));resources.duringTake=()->open.set(false);int takes=resources.takes;
                    try{manager.reauthorizeAndReserve("owner",request,TREE,67,open::get);throw new AssertionError("destroyed owner must not reserve");}catch(ExportManager.CleanupException expected){}
                    manager.destroy("owner");
                    check(resources.document.exists()&&journal.read()!=null&&resources.deletes==0,"destroy retried pending cleanup after interrupted reauthorization");
                    check(java.util.Arrays.equals(original,bytes(new File(metadata,"export-cleanup.json"))),"destroy altered pending ticket");
                    check(resources.takes-takes==(mode.equals("closed-before-take")?0:1),"closed owner acquired permission");
                    resources.held=3;resources.duringTake=()->{};manager.reserve("replacement-owner");check(!resources.document.exists()&&journal.read()==null&&resources.deletes==1&&resources.held==0,"next explicit request must recover retained ticket");manager.cancel("replacement-owner");
                }
                passed++;
            }finally{manager.worker.shutdownNow();remove(dir);}
        }
        remove(parent);return passed;
    }
}
