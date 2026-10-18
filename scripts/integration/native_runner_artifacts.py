"""Read retained mobile archives without extracting files or invoking tools."""
import hashlib
import plistlib
from pathlib import PurePosixPath
import stat
import struct
import zipfile
import zlib
from .native_results import require, read_json, digest


def _zip_entries(archive):
    rows=archive.infolist();require(len(rows)<=20000,'archive_too_large')
    require(len({r.filename for r in rows})==len(rows),'duplicate_archive_entry')
    require(sum(r.file_size for r in rows)<=1024*1024*1024 and all(r.file_size<=256*1024*1024 for r in rows),'archive_too_large')
    for row in rows:
        p=PurePosixPath(row.filename)
        require(not p.is_absolute() and '..' not in p.parts and ':' not in row.filename and '\\' not in row.filename,'unsafe_archive_path')
        require(stat.S_IFMT(row.external_attr>>16)!=stat.S_IFLNK,'linked_archive_entry')
        require(not row.flag_bits&1,'encrypted_archive_entry')
        name=row.filename.lower()
        require(not name.endswith(('.jks','.keystore','.p12','.pfx','.pem')) and 'owned_backend' not in name and 'fixture_credentials' not in name,'private_archive_asset')
    return rows


def android_artifact(out, result, file):
    from .android_build import apk_metadata, archive_manifest
    value=result.get('apk',{})
    require(value.get('path')=='android-release-test.apk','android_artifact_path_invalid')
    apk=file('android-release-test.apk',value.get('sha256'))
    manifest_path=file('apk-manifest.json',value.get('manifest_sha256'))
    with zipfile.ZipFile(apk) as archive:_zip_entries(archive)
    actual=archive_manifest(apk);require(actual==read_json(manifest_path),'apk_manifest_mismatch')
    metadata=apk_metadata((out/'badging.log').read_text(),(out/'signature.log').read_text())
    require(all(value.get(k)==v for k,v in metadata.items()) and value.get('test_fixture_classes_absent') is True,'apk_metadata_mismatch')
    return {'path':apk,'target':'android_apk','build_mode':'release','environment':'simulator','signing':'test_only'}


def _simulator_arm64(data):
    require(len(data)>=32,'ios_macho_invalid')
    magic=data[:4]
    if magic in (b'\xca\xfe\xba\xbe',b'\xbe\xba\xfe\xca',b'\xca\xfe\xba\xbf',b'\xbf\xba\xfe\xca'):
        endian='>' if magic in (b'\xca\xfe\xba\xbe',b'\xca\xfe\xba\xbf') else '<'
        count=struct.unpack_from(endian+'I',data,4)[0];wide=magic in (b'\xca\xfe\xba\xbf',b'\xbf\xba\xfe\xca');size=32 if wide else 20
        require(0<count<=16 and 8+count*size<=len(data),'ios_macho_invalid')
        selected=[]
        for i in range(count):
            pos=8+i*size;cpu=struct.unpack_from(endian+'I',data,pos)[0]
            offset,length=struct.unpack_from(endian+('QQ' if wide else 'II'),data,pos+8)
            require(offset+length<=len(data),'ios_macho_invalid')
            if cpu==0x100000c:selected.append(data[offset:offset+length])
        require(len(selected)==1,'ios_arm64_required');data=selected[0]
    require(data[:4]==b'\xcf\xfa\xed\xfe' and len(data)>=32,'ios_macho_invalid')
    _,cpu,_,_,count,command_bytes,_,_=struct.unpack_from('<IIIIIIII',data)
    require(cpu==0x100000c and 0<count<=1000 and 32+command_bytes<=len(data),'ios_arm64_required')
    offset=32;versions=[]
    for _ in range(count):
        require(offset+8<=32+command_bytes,'ios_macho_invalid');command,size=struct.unpack_from('<II',data,offset)
        require(size>=8 and offset+size<=32+command_bytes,'ios_macho_invalid')
        if command==0x32:
            require(size>=24,'ios_macho_invalid');versions.append(struct.unpack_from('<II',data,offset+8))
        offset+=size
    require(offset==32+command_bytes and versions==[(7,15<<16)],'ios_simulator_minimum_platform_mismatch')


def ios_artifact(out, result, file):
    bundle=result.get('bundle',{})
    archive=file('ios-simulator-debug.zip',result.get('archive_sha256'))
    manifest=read_json(file('bundle-manifest.json',bundle.get('manifest_sha256')))
    require(type(result.get('archive_actual_exit')) is int and result['archive_actual_exit']==0 and type(bundle.get('codesign_actual_exit')) is int and bundle['codesign_actual_exit']==0 and 'Signature=adhoc' in bundle.get('signature',''),'ios_archive_or_signature_unverified')
    with zipfile.ZipFile(archive) as z:
        rows=_zip_entries(z);actual={};info=None
        for row in rows:
            if row.is_dir() or row.filename.startswith('__MACOSX/'):continue
            require(row.filename.startswith('Runner.app/'),'ios_archive_root_invalid')
            name=row.filename[len('Runner.app/'):];h=hashlib.sha256()
            with z.open(row) as stream:
                for block in iter(lambda:stream.read(1024*1024),b''):h.update(block)
            actual[name]=h.hexdigest()
            if name=='Info.plist':info=plistlib.loads(z.read(row))
        require(actual==manifest and bundle.get('file_count')==len(manifest),'ios_bundle_manifest_mismatch')
        require(isinstance(info,dict) and info.get('CFBundleIdentifier')=='com.enterpriseim.client' and info.get('DTPlatformName')=='iphonesimulator' and info.get('MinimumOSVersion')=='15.0' and bundle.get('minimum_os_version')=='15.0','ios_bundle_platform_mismatch')
        exe=info.get('CFBundleExecutable');require(isinstance(exe,str) and PurePosixPath(exe).name==exe and exe in actual,'ios_executable_invalid')
        require(actual[exe]==bundle.get('executable_sha256'),'ios_executable_hash_mismatch')
        row=z.getinfo('Runner.app/'+exe);require(row.file_size<=32*1024*1024,'ios_executable_too_large');_simulator_arm64(z.read(row))
    return {'path':archive,'target':'ios_simulator','build_mode':'debug','environment':'simulator','signing':'test_only'}


def validate_png(data):
    """Require a complete non-interlaced 8-bit RGB(A) Android screenshot."""
    require(data.startswith(b'\x89PNG\r\n\x1a\n'),'release_screenshot_invalid')
    offset=8;chunks=[];compressed=[];size=None
    while offset<len(data):
        require(offset+12<=len(data),'release_screenshot_invalid')
        length=struct.unpack_from('>I',data,offset)[0];kind=data[offset+4:offset+8]
        require(length<=64*1024*1024 and offset+12+length<=len(data),'release_screenshot_invalid')
        body=data[offset+8:offset+8+length];crc=struct.unpack_from('>I',data,offset+8+length)[0]
        require(zlib.crc32(kind+body)==crc,'release_screenshot_invalid');chunks.append(kind)
        if kind==b'IHDR':
            require(chunks==[b'IHDR'] and length==13,'release_screenshot_invalid')
            width,height,bits,color,compression,filters,interlace=struct.unpack('>IIBBBBB',body)
            require(0<width<=8192 and 0<height<=8192 and bits==8 and color in (2,6) and (compression,filters,interlace)==(0,0,0),'release_screenshot_invalid')
            size=(width*(3 if color==2 else 4)+1,height)
        elif kind==b'IDAT':compressed.append(body)
        elif kind==b'IEND':require(length==0 and offset+12==len(data),'release_screenshot_invalid')
        offset+=12+length
    require(size and compressed and chunks[-1:]==[b'IEND'] and chunks.count(b'IEND')==1,'release_screenshot_invalid')
    stride,height=size;limit=stride*height;require(limit<=256*1024*1024,'release_screenshot_invalid')
    try:
        decoder=zlib.decompressobj();raw=decoder.decompress(b''.join(compressed),limit+1)
        require(len(raw)==limit and decoder.eof and not decoder.unused_data and not decoder.unconsumed_tail and all(raw[i*stride]<=4 for i in range(height)),'release_screenshot_invalid')
    except zlib.error:raise ValueError('release_screenshot_invalid') from None
