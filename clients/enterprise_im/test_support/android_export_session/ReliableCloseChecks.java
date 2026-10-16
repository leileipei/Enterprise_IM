package com.enterpriseim.client.tests;

import android.content.res.AssetFileDescriptor;
import android.os.ParcelFileDescriptor;
import com.enterpriseim.client.ExportSession;
import com.enterpriseim.client.ProviderOutput;
import java.io.*;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

// Real Android reliable descriptor status. The fixture waits for the reader's
// explicit terminal error before close; it does not claim to await arbitrary cloud commits.
public final class ReliableCloseChecks {
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

    public static void run(File target)throws Exception {
        ParcelFileDescriptor[] pipe=ParcelFileDescriptor.createReliablePipe();
        AssetFileDescriptor asset=new AssetFileDescriptor(pipe[1],0,AssetFileDescriptor.UNKNOWN_LENGTH);
        CountDownLatch reported=new CountDownLatch(1);Throwable[] peerFailure={null};
        Thread reader=new Thread(()->{
            try(FileOutputStream file=new FileOutputStream(target)) {
                FileInputStream input=new FileInputStream(pipe[0].getFileDescriptor());
                byte[] chunk=new byte[8192];int remaining=70000;
                while(remaining>0){int count=input.read(chunk,0,Math.min(remaining,chunk.length));if(count<0)throw new IOException("premature EOF");file.write(chunk,0,count);remaining-=count;}
                file.flush();pipe[0].closeWithError("controlled-provider-terminal-error");
            }catch(Throwable e){peerFailure[0]=e;}finally{reported.countDown();}
        },"owned-reliable-provider");reader.start();
        ExportSession session=new ExportSession(()->new FilterOutputStream(ProviderOutput.wrap(asset)){
            @Override public void write(byte[] b,int off,int len)throws IOException{out.write(b,off,len);}
            @Override public void flush()throws IOException{
                out.flush();try{if(!reported.await(10,TimeUnit.SECONDS))throw new IOException("provider deadline");}catch(InterruptedException e){throw new IOException(e);}
            }
        },()->!target.exists()||target.delete());
        Exception rejected=null;
        try {
            for(int offset=0;offset<70000;offset+=32768){byte[] bytes=new byte[Math.min(32768,70000-offset)];for(int i=0;i<bytes.length;i++)bytes[i]=(byte)((offset+i)%251);session.write(bytes);}
            try{session.finish();}catch(Exception e){rejected=e;}
            reader.join(10000);if(reader.isAlive()||peerFailure[0]!=null)throw new AssertionError("provider fixture failed",peerFailure[0]);
            if(target.length()!=70000)throw new AssertionError("real peer bytes missing");
            byte[] bytes=read(target);for(int i=0;i<bytes.length;i++)if(bytes[i]!=(byte)(i%251))throw new AssertionError("peer bytes changed");
            if(rejected==null||!rejected.toString().contains("controlled-provider-terminal-error"))throw new AssertionError("reported provider close error falsely accepted",rejected);
            if(pipe[1].getFileDescriptor().valid())throw new AssertionError("writer descriptor leaked");
            session.abort();if(target.exists())throw new AssertionError("reported provider error residual");System.out.println("SESSION_PASS reliable-provider-terminal-error");
        }finally{pipe[0].close();pipe[1].close();reader.join(10000);target.delete();}
    }
}
