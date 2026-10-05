#!/usr/bin/env python3
"""Generate bounded PNG fixtures in an explicitly supplied private directory."""
import argparse, os, struct, zlib
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument("output");args=p.parse_args()
d=Path(args.output);d.mkdir(mode=0o700,parents=True,exist_ok=True)
if not d.is_dir() or d.is_symlink() or d.stat().st_mode&0o777!=0o700:raise SystemExit("private0700 output required")
def chunk(t,b):return struct.pack(">I",len(b))+t+b+struct.pack(">I",zlib.crc32(t+b)&0xffffffff)
for name,width,height in [("40m.png",8000,5000),("40m-plus-one.png",40000001,1)]:
 with (d/name).open("wb") as f:
  os.chmod(d/name,0o600);f.write(b"\x89PNG\r\n\x1a\n");f.write(chunk(b"IHDR",struct.pack(">IIBBBBB",width,height,16,6,0,0,0)))
  if name=="40m.png":
   c=zlib.compressobj();data=bytearray();row=bytes(1+width*8)
   for _ in range(height):data.extend(c.compress(row))
   data.extend(c.flush());f.write(chunk(b"IDAT",bytes(data)))
  else:f.write(chunk(b"IDAT",zlib.compress(b"\0")))
  f.write(chunk(b"IEND",b""))
