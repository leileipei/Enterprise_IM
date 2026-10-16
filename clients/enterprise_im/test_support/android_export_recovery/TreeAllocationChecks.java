package com.enterpriseim.client.tests;

import android.net.Uri;
import android.database.MatrixCursor;
import android.os.Bundle;
import android.provider.DocumentsContract;
import com.enterpriseim.client.ExportDirectory;
import com.enterpriseim.client.ExportJournal;
import com.enterpriseim.client.ExportLedger;
import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;

/** Actual target state machine, Android AtomicFile and real directory/file effects. */
final class TreeAllocationChecks {
    private static byte[] read(File file) throws IOException {
        try (FileInputStream in = new FileInputStream(file); ByteArrayOutputStream out = new ByteArrayOutputStream()) {
            byte[] block = new byte[8192]; int count;
            while ((count = in.read(block)) != -1) out.write(block, 0, count);
            return out.toByteArray();
        }
    }
    private static void write(File file, byte[] bytes) throws IOException {
        try (FileOutputStream out = new FileOutputStream(file)) { out.write(bytes); }
    }

    private static final Uri TREE=Uri.parse("content://controlled.export/tree/root");
    private static final Uri DOC=Uri.parse("content://controlled.export/tree/root/document/owned");
    private static final Uri FOREIGN=Uri.parse("content://controlled.export/tree/root/document/foreign");
    private static final String TICKET="b3e8ce05-ae1d-4187-9f6d-8a1d188a319a", PREFIX="im-b3e8ce05-ae1d-4187-9f6d-8a1d188a319a-";
    private static void check(boolean value,String message){if(!value)throw new AssertionError(message);}
    private interface Action {void run()throws Exception;}
    private static void fails(Action action)throws Exception{boolean failed=false;try{action.run();}catch(Exception error){failed=true;}check(failed,"expected explicit failure");}
    private static final class Disk implements ExportLedger.Resources {
        final File dir,foreign,sentinel;File document;String name;int held;boolean loading,emptyListing,partialListing,queryError,afterCreate,defer,block,throwDelete,releaseFailure,takeFailure,unsupported;
        Disk(File dir)throws Exception{this.dir=dir;foreign=new File(dir,"unrelated.bin");sentinel=new File(dir,"sentinel");write(foreign,new byte[]{55});write(sentinel,new byte[]{31,41,59,26});}
        public int grants(Uri uri){check(uri.equals(TREE),"grant must be directory URI");return held;}
        public void take(Uri uri,int flags)throws Exception{held|=flags;if(takeFailure)throw new IOException("grant fault");}
        public void release(Uri uri,int flags)throws Exception{if(releaseFailure)throw new IOException("release fault");held&=~flags;}
        public void validateTree(Uri tree)throws Exception{if(unsupported)throw new IOException("not creatable");}
        public void create(Uri tree,String name)throws Exception{this.name=name;document=new File(dir,name);if(defer)throw new IOException("creation outcome unknown");write(document,new byte[0]);if(afterCreate)throw new IOException("caller lost before create return");}
        public Uri findAllocated(Uri tree,String prefix)throws Exception{
            File[] matches=dir.listFiles((d,n)->n.startsWith(prefix));Arrays.sort(matches);
            try(MatrixCursor cursor=new MatrixCursor(new String[]{"document_id","_display_name","mime_type"})){
                if(!emptyListing)for(int i=0;i<matches.length;i++){
                    if(partialListing&&i>0)break;
                    cursor.addRow(new Object[]{matches[i].equals(document)?"owned":"extra",matches[i].getName(),"application/octet-stream"});
                }
                Bundle extras=new Bundle();if(loading)extras.putBoolean(DocumentsContract.EXTRA_LOADING,true);if(queryError)extras.putString(DocumentsContract.EXTRA_ERROR,"query failed");cursor.setExtras(extras);
                return ExportDirectory.findAllocated(cursor,tree,prefix);
            }
        }
        public boolean delete(Uri uri)throws Exception{if(throwDelete)throw new IOException("delete fault");if(block)return false;if(uri.equals(FOREIGN))return foreign.delete();if(!uri.equals(DOC))throw new IOException("unknown URI");return document!=null&&document.delete();}
        public boolean absent(Uri uri){return uri.equals(FOREIGN)?!foreign.exists():document==null||!document.exists();}
        void protect()throws Exception{check(Arrays.equals(read(foreign),new byte[]{55}),"unrelated file changed");check(Arrays.equals(read(sentinel),new byte[]{31,41,59,26}),"sentinel changed");}
    }
    private static void remove(File file)throws Exception{if(file.isDirectory())for(File child:file.listFiles())remove(child);check(file.delete(),"test cleanup");}
    static int run(File parent)throws Exception {
        check(parent.mkdir(),"fresh test directory");List<String> passed=new ArrayList<>();
        try {
            for(String mode:new String[]{"created-before-return","deferred-creation","delete-false","delete-throws","preexisting-directory-grant","prefix-collision","multiple-matches","committed-file","deleted-release-retry","partial-grant-failure","unsupported-directory","utf8-leaf-name","foreign-uri-record","loading-empty","loading-partial-one","error-result"}){
                File dir=new File(parent,mode);check(dir.mkdir(),"fresh case");Disk disk=new Disk(dir);File metadata=new File(dir,"metadata");check(metadata.mkdir(),"metadata dir");ExportJournal journal=new ExportJournal(metadata);ExportLedger ledger=new ExportLedger(journal,disk);
                if(mode.equals("preexisting-directory-grant"))disk.held=1;
                if(mode.equals("created-before-return"))disk.afterCreate=true;
                if(mode.equals("deferred-creation"))disk.defer=true;
                if(mode.equals("partial-grant-failure"))disk.takeFailure=true;
                if(mode.equals("unsupported-directory"))disk.unsupported=true;
                File collision=new File(dir,PREFIX+"preexisting.bin");
                if(mode.equals("prefix-collision"))write(collision,new byte[]{21});
                String requested=mode.equals("utf8-leaf-name")?new String(new char[80]).replace('\0', '文')+".xlsx":"report.bin";
                if(disk.afterCreate||disk.defer||disk.takeFailure||disk.unsupported||collision.exists())fails(()->ledger.allocateTree(TICKET,TREE,requested,67));
                else check(ledger.allocateTree(TICKET,TREE,requested,67).equals(DOC),"allocated URI");
                if(mode.equals("deferred-creation")){fails(ledger::recover);check(journal.read().phase.equals("ALLOCATING"),"absent allocation must stay pending");write(disk.document,new byte[0]);}
                if(mode.equals("delete-false")||mode.equals("delete-throws")){disk.block=true;disk.throwDelete=mode.equals("delete-throws");fails(ledger::recover);check(disk.document.exists()&&journal.read()!=null,"delete fault retained");disk.block=false;disk.throwDelete=false;}
                File extra=new File(dir,PREFIX+"second.bin");
                if(mode.equals("multiple-matches")){write(extra,new byte[]{22});fails(ledger::recover);check(extra.exists()&&disk.document.exists(),"ambiguous files must both survive");check(extra.delete(),"test reset");}
                if(mode.equals("loading-empty")||mode.equals("loading-partial-one")||mode.equals("error-result")){
                    disk.loading=!mode.equals("error-result");disk.emptyListing=!mode.equals("loading-partial-one");disk.partialListing=mode.equals("loading-partial-one");disk.queryError=mode.equals("error-result");
                    if(disk.partialListing)write(extra,new byte[]{22});
                    fails(ledger::recover);check(disk.document.exists()&&journal.read()!=null&&disk.held==3,"incomplete listing must retain file, journal and grant");
                    if(disk.partialListing){check(extra.exists(),"unseen matching file preserved");check(extra.delete(),"test reset");}
                    disk.loading=false;disk.emptyListing=false;disk.partialListing=false;disk.queryError=false;
                }
                if(mode.equals("committed-file")){write(disk.document,new byte[]{5,8,13});ledger.commit(TICKET);}
                if(mode.equals("deleted-release-retry")){disk.releaseFailure=true;fails(ledger::recover);check(journal.read().phase.equals("DELETED")&&!disk.document.exists(),"receipt before release");write(disk.document,new byte[]{42});disk.releaseFailure=false;}
                if(mode.equals("utf8-leaf-name"))check(disk.name.startsWith(PREFIX)&&disk.name.endsWith(".xlsx")&&disk.name.getBytes(StandardCharsets.UTF_8).length<=255&&disk.document.exists(),"bounded real UTF8 filename");
                if(mode.equals("foreign-uri-record")){
                    ExportJournal.Entry entry=journal.read();journal.write(new ExportJournal.Entry(entry.ticket,FOREIGN,entry.grantUri,entry.acquired,"PENDING",entry.prefix,entry.displayName));
                    fails(ledger::recover);check(disk.document.exists()&&disk.foreign.exists()&&journal.read()!=null,"corrupted URI must not delete another file");
                }else{
                    new ExportLedger(new ExportJournal(metadata),disk).recover();check(journal.read()==null,"journal cleared after confirmed recovery");
                    check(disk.held==(mode.equals("preexisting-directory-grant")?1:0),"only own grant released");
                    if(mode.equals("committed-file"))check(Arrays.equals(read(disk.document),new byte[]{5,8,13}),"committed file preserved");
                    else if(mode.equals("deleted-release-retry"))check(Arrays.equals(read(disk.document),new byte[]{42}),"receipt prevents second deletion");
                    else check(disk.document==null||!disk.document.exists(),"owned allocation removed");
                    if(mode.equals("prefix-collision"))check(Arrays.equals(read(collision),new byte[]{21}),"preexisting prefix collision preserved");
                }
                disk.protect();remove(dir);passed.add(mode);
            }
            remove(parent);android.util.Log.i("EnterpriseIMTest","TREE_ALLOCATION_PASSED "+passed);return passed.size();
        }catch(Throwable failure){throw new AssertionError("TREE_ALLOCATION passed="+passed+" root="+parent,failure);}
    }
}
