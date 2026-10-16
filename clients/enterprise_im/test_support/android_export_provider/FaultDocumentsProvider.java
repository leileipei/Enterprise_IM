package com.enterpriseim.exportfixture;

import android.database.Cursor;
import android.database.MatrixCursor;
import android.os.CancellationSignal;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.ParcelFileDescriptor;
import android.os.ProxyFileDescriptorCallback;
import android.os.storage.StorageManager;
import android.provider.DocumentsContract.Document;
import android.provider.DocumentsContract.Root;
import android.provider.DocumentsProvider;
import android.system.ErrnoException;
import android.system.OsConstants;
import java.io.*;
import java.util.*;

// Test APK only. Uses Binder, DocumentsUI and real app-private files, with
// deliberate OS-level write EIO and delete rejection; never packaged in the client.
public final class FaultDocumentsProvider extends DocumentsProvider {
    private File base;
    private Handler handler;
    @Override public boolean onCreate() {
        base = new File(getContext().getFilesDir(), "documents"); base.mkdirs();
        HandlerThread thread = new HandlerThread("export-fixture-io"); thread.start(); handler = new Handler(thread.getLooper());
        try { File sentinel = new File(base, "sentinel"); if (!sentinel.exists()) try (FileOutputStream out = new FileOutputStream(sentinel)) { out.write(new byte[]{31,41,59,26}); } } catch (IOException e) { throw new IllegalStateException(e); }
        return true;
    }
    private synchronized void event(String value) {
        try (FileWriter out = new FileWriter(new File(getContext().getFilesDir(), "events.log"), true)) { out.write(System.currentTimeMillis()+" pid="+android.os.Process.myPid()+" "+value+"\n"); }
        catch (IOException e) { throw new IllegalStateException(e); }
    }
    private File data(String id) throws FileNotFoundException {
        if (!id.matches("[a-f0-9-]{36}")) throw new FileNotFoundException("invalid id");
        return new File(base,id+".bin");
    }
    private String name(String id) throws FileNotFoundException {
        try { return new String(java.nio.file.Files.readAllBytes(new File(base,id+".name").toPath()), java.nio.charset.StandardCharsets.UTF_8); }
        catch(IOException e) { throw new FileNotFoundException("missing metadata"); }
    }
    @Override public boolean isChildDocument(String parent, String child) {
        return parent.equals("root") && child.matches("[a-f0-9-]{36}") && new File(base,child+".name").exists();
    }
    @Override public Cursor queryRoots(String[] projection) {
        MatrixCursor c = new MatrixCursor(projection != null ? projection : new String[]{Root.COLUMN_ROOT_ID,Root.COLUMN_DOCUMENT_ID,Root.COLUMN_TITLE,Root.COLUMN_FLAGS,Root.COLUMN_MIME_TYPES,Root.COLUMN_AVAILABLE_BYTES});
        c.newRow().add(Root.COLUMN_ROOT_ID,"root").add(Root.COLUMN_DOCUMENT_ID,"root").add(Root.COLUMN_TITLE,"IM Export Faults").add(Root.COLUMN_FLAGS,Root.FLAG_SUPPORTS_CREATE|Root.FLAG_LOCAL_ONLY|Root.FLAG_SUPPORTS_IS_CHILD).add(Root.COLUMN_MIME_TYPES,"*/*").add(Root.COLUMN_AVAILABLE_BYTES,100000000L); return c;
    }
    private void row(MatrixCursor c,String id) throws FileNotFoundException {
        boolean root=id.equals("root"); File f=root?base:data(id);
        c.newRow().add(Document.COLUMN_DOCUMENT_ID,id).add(Document.COLUMN_DISPLAY_NAME,root?"IM Export Faults":name(id)).add(Document.COLUMN_MIME_TYPE,root?Document.MIME_TYPE_DIR:"application/octet-stream").add(Document.COLUMN_FLAGS,root?Document.FLAG_DIR_SUPPORTS_CREATE:Document.FLAG_SUPPORTS_WRITE|Document.FLAG_SUPPORTS_DELETE).add(Document.COLUMN_SIZE,root?0:f.length()).add(Document.COLUMN_LAST_MODIFIED,f.lastModified());
    }
    private MatrixCursor cursor(String[] projection) { return new MatrixCursor(projection != null ? projection:new String[]{Document.COLUMN_DOCUMENT_ID,Document.COLUMN_DISPLAY_NAME,Document.COLUMN_MIME_TYPE,Document.COLUMN_FLAGS,Document.COLUMN_SIZE,Document.COLUMN_LAST_MODIFIED}); }
    @Override public Cursor queryDocument(String id,String[] projection) throws FileNotFoundException { MatrixCursor c=cursor(projection);row(c,id);return c; }
    @Override public Cursor queryChildDocuments(String parent,String[] projection,String sortOrder) throws FileNotFoundException {
        if(!parent.equals("root")) throw new FileNotFoundException(); MatrixCursor c=cursor(projection);File[] files=base.listFiles((d,n)->n.endsWith(".name"));if(files!=null) for(File f:files)row(c,f.getName().replace(".name",""));
        File loading=new File(getContext().getFilesDir(),"directory-loading");
        if(loading.exists()){
            android.os.Bundle extras=new android.os.Bundle();extras.putBoolean(android.provider.DocumentsContract.EXTRA_LOADING,true);c=cursor(projection);c.setExtras(extras);event("directory_loading empty=true");
        }
        return c;
    }
    @Override public String createDocument(String parent,String mime,String displayName) throws FileNotFoundException {
        if(!parent.equals("root") || !displayName.matches("(?:im-[a-f0-9-]{36}-)?im-[a-f0-9]{32}-(write|delete|write-delete|recovery|handoff-delete)\\.bin")) throw new FileNotFoundException("fixture name required");
        String id=UUID.randomUUID().toString();try { if(!data(id).createNewFile())throw new IOException();java.nio.file.Files.write(new File(base,id+".name").toPath(),displayName.getBytes(java.nio.charset.StandardCharsets.UTF_8));if(displayName.endsWith("delete.bin"))new File(base,id+".blocked").createNewFile(); }catch(IOException e){throw new FileNotFoundException(e.toString());}
        event("create id="+id+" name="+displayName);
        if(displayName.contains("-handoff-")) {
            File gate=new File(base,id+".return-wait");
            try {gate.createNewFile();}catch(IOException e){throw new FileNotFoundException();}
            event("create_held id="+id);
            long deadline=android.os.SystemClock.elapsedRealtime()+120000;
            while(gate.exists()&&android.os.SystemClock.elapsedRealtime()<deadline) {
                try {Thread.sleep(20);}catch(InterruptedException e){Thread.currentThread().interrupt();throw new FileNotFoundException();}
            }
            if(gate.exists())throw new FileNotFoundException("fixture return gate timed out");
            event("create_return id="+id);
        }
        return id;
    }
    @Override public ParcelFileDescriptor openDocument(String id,String mode,CancellationSignal signal) throws FileNotFoundException {
        File f=data(id);String n=name(id);if(mode.equals("r"))return ParcelFileDescriptor.open(f,ParcelFileDescriptor.MODE_READ_ONLY);
        event("open id="+id+" name="+n);final RandomAccessFile output;
        try { output=new RandomAccessFile(f,"rw");output.setLength(0); }catch(IOException e){throw new FileNotFoundException(e.toString());}
        try { return getContext().getSystemService(StorageManager.class).openProxyFileDescriptor(ParcelFileDescriptor.MODE_WRITE_ONLY,new ProxyFileDescriptorCallback(){
            @Override public long onGetSize(){return f.length();}
            @Override public int onWrite(long offset,int size,byte[] bytes) throws ErrnoException {
                if(n.contains("-write") && offset>=4096){event("write_error id="+id+" offset="+offset);throw new ErrnoException("fixture-write",OsConstants.EIO);}
                int count=n.contains("-write")?Math.min(size,(int)(4096-offset)):size;
                try {output.seek(offset);output.write(bytes,0,count);event("write id="+id+" offset="+offset+" size="+count);return count;}catch(IOException e){throw new ErrnoException("fixture-file",OsConstants.EIO);}
            }
            @Override public void onFsync() throws ErrnoException {try{output.getFD().sync();event("fsync id="+id);}catch(IOException e){throw new ErrnoException("fixture-fsync",OsConstants.EIO);}}
            @Override public void onRelease(){try{output.close();event("release id="+id);}catch(IOException e){event("release_error id="+id);}}
        },handler); }catch(IOException e){try{output.close();}catch(IOException ignored){}throw new FileNotFoundException(e.toString());}
    }
    @Override public void deleteDocument(String id) throws FileNotFoundException {
        File f=data(id);event("delete_attempt id="+id);
        if(new File(base,id+".blocked").exists()){event("delete_error id="+id);throw new FileNotFoundException("fixture persistent delete failure");}
        if(f.exists()&&!f.delete())throw new FileNotFoundException("cannot delete");new File(base,id+".name").delete();event("deleted id="+id);
    }
}
