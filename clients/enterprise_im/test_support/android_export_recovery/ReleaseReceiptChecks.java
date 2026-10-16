package com.enterpriseim.client.tests;

import android.net.Uri;
import com.enterpriseim.client.ExportJournal;
import com.enterpriseim.client.ExportLedger;
import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import org.json.*;

/** Real target state machine/AtomicFile/disk; controls only OS grant and boot boundaries. */
final class ReleaseReceiptChecks {
    private static void check(boolean ok,String message){if(!ok)throw new AssertionError(message);}
    private static final Uri URI=Uri.parse("content://controlled.release/document/owned");
    private static final String TICKET="10916e8a-8680-4481-9682-e448654a1c7f";
    private interface Action{void run()throws Exception;}
    private static void fails(Action action)throws Exception{boolean failed=false;try{action.run();}catch(Exception expected){failed=true;}check(failed,"operation must fail closed");}
    private static final class Grants implements ExportLedger.Resources {
        final Map<Uri,Integer> held=new HashMap<>();int boot=1,deletes,takes;boolean releaseFailure,takeFailure;final File file;
        Grants(File dir)throws Exception{file=new File(dir,"completed.bin");write(file,new byte[]{7,11,13});}
        public int bootCount(){return boot;}
        public int grants(Uri uri){return held.getOrDefault(uri,0);}
        public void take(Uri uri,int flags)throws Exception{takes++;held.put(uri,grants(uri)|(takeFailure?1:flags));if(takeFailure)throw new IOException("partial take");}
        public void release(Uri uri,int flags)throws Exception{if(releaseFailure)throw new IOException("release failure");held.put(uri,grants(uri)&~flags);}
        public boolean delete(Uri uri){deletes++;return file.delete();}
        public boolean absent(Uri uri){return !file.exists();}
    }
    private static final class Case {
        final File root,metadata,receipt;final Grants grants;final ExportJournal journal;ExportLedger ledger;
        Case(File parent,String name)throws Exception{root=new File(parent,name);check(root.mkdir(),"fresh case");metadata=new File(root,"metadata");check(metadata.mkdir(),"fresh metadata");receipt=new File(metadata,"export-grant-releases.json");grants=new Grants(root);journal=new ExportJournal(metadata);reload();}
        void reload(){ledger=new ExportLedger(new ExportJournal(metadata),grants);}
        void complete(Uri uri)throws Exception{ledger.prepare(TICKET,uri,67);ledger.commit(TICKET);ledger.recover();check(journal.read()==null,"terminal journal cleared");}
        JSONArray entries()throws Exception{return new JSONObject(new String(read(receipt),StandardCharsets.UTF_8)).getJSONArray("entries");}
        void protect()throws Exception{check(grants.deletes==0&&Arrays.equals(read(grants.file),new byte[]{7,11,13}),"permission reconciliation deleted or altered completed data");}
    }
    private static byte[] read(File file)throws Exception{try(InputStream in=new FileInputStream(file);ByteArrayOutputStream out=new ByteArrayOutputStream()){byte[] block=new byte[1024];int n;while((n=in.read(block))!=-1)out.write(block,0,n);return out.toByteArray();}}
    private static void write(File file,byte[] data)throws Exception{try(FileOutputStream out=new FileOutputStream(file)){out.write(data);out.getFD().sync();}}
    private static void remove(File f){if(f.isDirectory())for(File c:f.listFiles())remove(c);check(f.delete(),"fixture teardown");}
    static int run(File root)throws Exception{
        check(root.mkdir(),"fresh release fixture");List<String> passed=new ArrayList<>();
        for(String mode:new String[]{"resurrection","same-boot-retain","later-boot-prune","repeated-release-boot","preexisting-read","partial-take","unknown-boot","merge","capacity","capacity-tree","capacity-reauthorize","write-failure","pending-PREPARED","pending-TREE_PREPARED","pending-ALLOCATING","pending-PENDING","corrupt","unknown","orphan-new","atomic-interruption"}){
            Case c=new Case(root,mode);
            try{
                if(mode.equals("corrupt")||mode.equals("unknown")||mode.equals("orphan-new")||mode.equals("write-failure")){
                    File target=mode.equals("orphan-new")?new File(c.receipt+".new"):c.receipt;
                    if(mode.equals("write-failure"))check(target.mkdir(),"metadata path obstruction");
                    else write(target,(mode.equals("unknown")?"{\"version\":99,\"entries\":[]}":"invalid").getBytes(StandardCharsets.UTF_8));
                    fails(()->c.ledger.prepare(TICKET,URI,67));check(c.grants.takes==0&&c.journal.read()==null&&target.exists(),"invalid release metadata did not block before take");
                }else if(mode.startsWith("capacity")){
                    for(int i=0;i<32;i++)c.complete(Uri.parse("content://controlled.release/document/owned-"+i));
                    int takes=c.grants.takes;
                    if(mode.equals("capacity-reauthorize")){
                        Uri tree=Uri.parse("content://controlled.release/tree/other");c.journal.write(new ExportJournal.Entry(TICKET,Uri.parse("content://controlled.release/tree/other/document/owned"),tree,0,"PENDING","im-"+TICKET+"-","im-"+TICKET+"-report.bin"));
                        byte[] original=read(new File(c.metadata,"export-cleanup.json"));ExportLedger.Reauthorization request=c.ledger.authorizationRequired();check(request!=null,"legacy grant-loss recovery challenge");fails(()->c.ledger.reauthorize(request,tree,67));check(Arrays.equals(original,read(new File(c.metadata,"export-cleanup.json")))&&c.grants.takes==takes,"reauthorization capacity checked after metadata/take");
                        c.protect();passed.add(mode);continue;
                    }
                    if(mode.equals("capacity-tree"))fails(()->c.ledger.allocateTree(TICKET,Uri.parse("content://controlled.release/tree/other"),"report.bin",67));else fails(()->c.ledger.prepare(TICKET,URI,67));
                    check(c.grants.takes==takes&&c.journal.read()==null&&c.entries().length()==32,"capacity failed after journal/take");
                    c.complete(Uri.parse("content://controlled.release/document/owned-0"));check(c.entries().length()==32,"same URI must merge at capacity");c.grants.boot=2;c.reload();c.ledger.recover();check(c.entries().length()==0,"mature receipts must free capacity");c.complete(URI);
                }else{
                    if(mode.equals("preexisting-read"))c.grants.held.put(URI,1);
                    if(mode.equals("partial-take")){
                        c.grants.takeFailure=true;fails(()->c.ledger.prepare(TICKET,URI,67));check(c.entries().getJSONObject(0).getInt("owned")==3&&c.grants.grants(URI)==1,"partial take not owned before failure");c.grants.takeFailure=false;c.journal.write(new ExportJournal.Entry(TICKET,URI,3,"COMMITTED"));c.ledger.recover();check(c.grants.grants(URI)==0&&c.journal.read()==null,"partial take responsibility not released");
                    }else c.complete(URI);
                    if(mode.equals("resurrection")||mode.equals("preexisting-read")){
                        c.grants.boot=2;c.grants.held.put(URI,3);c.reload();c.ledger.recover();check(c.grants.grants(URI)==(mode.equals("preexisting-read")?1:0),"owned grant resurrected after completed release was not reconciled");check(c.entries().getJSONObject(0).getInt("lastReleaseAttemptBoot")==2,"new release attempt boot not durable");
                    }else if(mode.equals("same-boot-retain")){c.reload();c.ledger.recover();check(c.entries().length()==1,"receipt discarded before OS reboot confirmation");}
                    else if(mode.equals("later-boot-prune")){c.grants.boot=2;c.reload();c.ledger.recover();check(c.entries().length()==0,"later boot absent grant not pruned");}
                    else if(mode.equals("repeated-release-boot")){
                        c.grants.boot=2;c.grants.held.put(URI,3);c.grants.releaseFailure=true;c.reload();fails(c.ledger::recover);check(c.entries().getJSONObject(0).getInt("lastReleaseAttemptBoot")==2,"attempt boot not saved before release failure");c.grants.releaseFailure=false;c.ledger.recover();c.reload();c.ledger.recover();check(c.entries().length()==1,"same-boot premature prune after retry");c.grants.boot=3;c.grants.held.put(URI,3);c.ledger.recover();check(c.grants.grants(URI)==0&&c.entries().getJSONObject(0).getInt("lastReleaseAttemptBoot")==3,"second resurrection not retried");
                    }else if(mode.equals("unknown-boot")){c.grants.boot=-1;c.ledger.recover();check(c.entries().length()==1,"unknown boot discarded receipt");c.grants.held.put(URI,3);c.ledger.recover();check(c.grants.grants(URI)==0&&c.entries().length()==1,"unknown boot failed safe release/retention");}
                    else if(mode.equals("merge")){for(int i=0;i<3;i++)c.complete(URI);check(c.entries().length()==1&&c.entries().getJSONObject(0).getInt("owned")==3,"same URI obligations not merged");}
                    else if(mode.startsWith("pending-")){
                        String phase=mode.substring(8);Uri tree=Uri.parse("content://controlled.release/tree/root");
                        // Seed a legitimate old release obligation for the same grant URI.
                        c.complete(tree);c.grants.held.put(tree,3);c.grants.boot=2;
                        if(phase.equals("PREPARED"))c.journal.write(new ExportJournal.Entry(TICKET,tree,3,"PREPARED"));
                        else c.journal.write(new ExportJournal.Entry(TICKET,phase.equals("PENDING")?Uri.parse("content://controlled.release/tree/root/document/owned"):Uri.EMPTY,tree,3,phase,"im-"+TICKET+"-","im-"+TICKET+"-report.bin"));
                        byte[] original=read(new File(c.metadata,"export-cleanup.json"));c.reload();c.ledger.reconcileReleasedGrants();check(c.grants.grants(tree)==3&&Arrays.equals(original,read(new File(c.metadata,"export-cleanup.json"))),"startup released access needed by durable unfinished journal");
                    }else if(mode.equals("atomic-interruption")){
                        write(new File(c.receipt+".new"),"incomplete".getBytes(StandardCharsets.UTF_8));c.grants.boot=2;c.reload();c.ledger.recover();check(c.entries().length()==0&&!new File(c.receipt+".new").exists(),"atomic sidecar recovery failed");
                    }
                }
                c.protect();passed.add(mode);
            }catch(Throwable error){throw new AssertionError("release receipts mode="+mode+" passed="+passed,error);}
            finally{remove(c.root);}
        }
        remove(root);android.util.Log.i("EnterpriseIMTest","RELEASE_RECEIPTS_PASSED "+passed);return passed.size();
    }
}
