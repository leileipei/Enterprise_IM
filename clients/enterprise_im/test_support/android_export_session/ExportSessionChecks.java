package com.enterpriseim.client.tests;

import com.enterpriseim.client.ExportSession;
import java.io.*;
import java.util.Arrays;

// Same contract checks run on the host and against the target app's class on Android.
// Stream faults live only here; they wrap real files and throw synchronous IOException.
public final class ExportSessionChecks {
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

    private static void require(boolean value,String message){if(!value)throw new AssertionError(message);}
    private static final byte[] PAYLOAD=new byte[70000];
    static {for(int i=0;i<PAYLOAD.length;i++)PAYLOAD[i]=(byte)(i%251);}
    private static final class FaultOutput extends OutputStream {
        final FileOutputStream file;final boolean writeFault,closeFault;boolean closed;
        FaultOutput(File path,boolean writeFault,boolean closeFault)throws IOException{file=new FileOutputStream(path);this.writeFault=writeFault;this.closeFault=closeFault;}
        @Override public void write(int value)throws IOException{file.write(value);}
        @Override public void write(byte[] data,int offset,int size)throws IOException{if(writeFault){file.write(data,offset,4096);throw new IOException("controlled-write");}file.write(data,offset,size);}
        @Override public void flush()throws IOException{file.flush();}
        @Override public void close()throws IOException{closed=true;file.close();if(closeFault)throw new IOException("controlled-close");}
    }
    private static void feed(ExportSession session)throws IOException{for(int offset=0;offset<PAYLOAD.length;offset+=32768)session.write(Arrays.copyOfRange(PAYLOAD,offset,Math.min(offset+32768,PAYLOAD.length)));}
    private static Exception rejected(Work work)throws Exception{try{work.run();}catch(Exception e){return e;}throw new AssertionError("operation falsely reported success");}
    private interface Work{void run()throws Exception;}
    private static boolean delete(File target){return !target.exists()||target.delete();}
    public static int run(File root)throws Exception{
        require(root.mkdir(),"fresh owned check directory required");int passed=0;
        File sentinel=new File(root,"sentinel");write(sentinel,new byte[]{31,41,59,26});
        try{
            for(String mode:new String[]{"write","close","write-close"}){
                File target=new File(root,mode);FaultOutput[] output={null};boolean w=mode.contains("write"),c=mode.contains("close");
                ExportSession session=new ExportSession(()->output[0]=new FaultOutput(target,w,c),()->delete(target));feed(session);
                Exception error=rejected(session::finish);require(error.getMessage().equals(w?"controlled-write":"controlled-close"),"original I/O error lost");
                require(output[0].closed&&!output[0].file.getFD().valid(),"descriptor leaked");require(target.length()==(w?4096:70000),"fault did not occur after real bytes");
                if(w&&c)require(error.getSuppressed().length==1&&error.getSuppressed()[0].getMessage().equals("controlled-close"),"close error must be suppressed on write error");
                session.abort();require(!target.exists(),"failed final copy residual");passed++;System.out.println("SESSION_PASS "+mode);
            }
            for(boolean falseResult:new boolean[]{false,true}){
                File target=new File(root,falseResult?"delete-false":"delete-throws");boolean[] allowed={false};FaultOutput[] output={null};
                ExportSession session=new ExportSession(()->output[0]=new FaultOutput(target,false,true),()->{if(!allowed[0]){if(falseResult)return false;throw new IOException("controlled-delete");}return delete(target);});feed(session);rejected(session::finish);
                rejected(session::abort);require(target.exists()&&target.length()==70000,"pending owned target must remain for retry");
                rejected(()->session.write(new byte[]{99}));rejected(session::finish);
                allowed[0]=true;session.abort();require(!target.exists(),"retry did not remove same owned target");session.abort();passed++;System.out.println("SESSION_PASS "+(falseResult?"delete-false":"delete-throws"));
            }
            File target=new File(root,"complete");ExportSession ok=new ExportSession(()->new FileOutputStream(target),()->delete(target));feed(ok);ok.finish();require(Arrays.equals(read(target),PAYLOAD),"successful copy changed bytes");passed++;System.out.println("SESSION_PASS success");
            File large=new File(root,"maximum");boolean[] opened={false};
            ExportSession maximum=new ExportSession(()->{opened[0]=true;return new FileOutputStream(large);},()->delete(large));
            byte[] block=new byte[32768];
            for(int i=0;i<800;i++){Arrays.fill(block,(byte)(i%251));maximum.write(block);}
            Arrays.fill(block,(byte)99);
            require(!opened[0]&&!large.exists(),"source must remain private until finish");
            rejected(()->maximum.write(new byte[]{1}));maximum.finish();
            require(large.length()==26214400L,"maximum export size changed");
            try(FileInputStream in=new FileInputStream(large)){
                for(int i=0;i<800;i++){
                    int offset=0;while(offset<block.length){int n=in.read(block,offset,block.length-offset);require(n>0,"maximum export truncated");offset+=n;}
                    for(byte value:block)require(value==(byte)(i%251),"maximum export bytes or ownership changed");
                }
                require(in.read()==-1,"maximum export grew");
            }
            rejected(maximum::finish);rejected(()->maximum.write(new byte[]{1}));passed++;System.out.println("SESSION_PASS maximum-25MiB-owned-buffer");
            require(Arrays.equals(read(sentinel),new byte[]{31,41,59,26}),"unrelated sentinel changed");return passed;
        }finally{for(File f:root.listFiles())require(f.delete(),"test-owned fixture cleanup");require(root.delete(),"owned root cleanup");}
    }
    public static void main(String[] args)throws Exception{int n=run(new File(args[0]));require(n==7,"expected seven contracts");System.out.println("SESSION_ALL_PASSED "+n);}
}
