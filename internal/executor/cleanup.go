package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
)

func (e *Engine) cleanup(ctx context.Context, job Job, result Result) Result {
	opts := job.Request.Cleanup
	script := "set -Eeuo pipefail\numask 077\n" + diagnosticPrelude + "msboost_phase=cleanup\ncommand -v python3 >/dev/null || { printf 'MSBOOST_ERROR_CODE=missing_dependency\\n'; exit 1; }\n" +
		encodedAssignment("cleanup_scope", opts.Scope) + encodedAssignment("cleanup_digest", opts.Digest) + encodedAssignment("cleanup_action", job.Request.Kind) + encodedAssignment("cleanup_task", opts.ManagedTaskID) +
		"python3 - \"$cleanup_scope\" \"$cleanup_digest\" \"$cleanup_action\" \"$cleanup_task\" <<'MSBOOST_CLEANUP_PY'\n" + cleanupPython + "\nMSBOOST_CLEANUP_PY\n"
	out, err := e.Remote.Run(ctx, job.Request.SSH, script)
	if err != nil {
		return failRemote(result, "cleanup", out, err)
	}
	raw, err := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CLEANUP"))
	var report CleanupReport
	if err != nil || json.Unmarshal(raw, &report) != nil || !ValidCleanupReport(&report, opts.Scope, job.Request.Kind == "cleanup") || report.ManagedTaskID != opts.ManagedTaskID {
		return failRemote(result, "cleanup", nil, diagnosticError("cleanup_failed"))
	}
	result.State, result.Phase, result.Cleanup = "succeeded", "complete", &report
	return result
}

// Paths are a closed public inventory; raw filesystem contents never enter task records.
func ValidCleanupReport(report *CleanupReport, scope string, removed bool) bool {
	if report == nil || report.Scope != scope || report.Removed != removed || !validSHA(report.Digest) || len(report.Items) > 2000 {
		return false
	}
	if report.ManagedTaskID != "" && (scope != "relay" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(report.ManagedTaskID)) {
		return false
	}
	for _, item := range report.Items {
		if !validCleanupItem(scope, item) {
			return false
		}
		if report.ManagedTaskID != "" && (scope != "relay" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(report.ManagedTaskID) || item.Kind != "firewall" && item.Path != "/etc/msboost-free/"+report.ManagedTaskID && item.Path != "/etc/systemd/system/msboost-free-"+report.ManagedTaskID+".service") {
			return false
		}
	}
	return true
}

var cleanupRelayPath = regexp.MustCompile(`^(/etc/msboost-free/[A-Za-z0-9_-]{1,100}|/etc/systemd/system/msboost-free-[A-Za-z0-9_-]{1,100}\.service)$`)
var cleanupFirewall = regexp.MustCompile(`^(ufw::|runtime:[A-Za-z0-9_.-]{1,80}:|permanent:[A-Za-z0-9_.-]{1,80}:)[0-9]{1,5}/tcp$`)

func validCleanupItem(scope string, item CleanupItem) bool {
	if item.Kind == "firewall" {
		return cleanupFirewall.MatchString(item.Path)
	}
	if item.Kind != "file" && item.Kind != "directory" {
		return false
	}
	if scope == "relay" {
		return cleanupRelayPath.MatchString(item.Path)
	}
	for _, path := range []string{"/etc/msboost", "/var/lib/msboost", "/usr/local/bin/msboost", "/etc/systemd/system/msboost.service", "/root/直连.json", "/etc/msboost/relay_tcp_probe.py", "/etc/msboost/tcp-probe.json", "/etc/systemd/system/msboost-tcp-probe.service"} {
		if item.Path == path {
			expectedKind := "file"
			if path == "/etc/msboost" || path == "/var/lib/msboost" {
				expectedKind = "directory"
			}
			return item.Kind == expectedKind
		}
	}
	return false
}

const cleanupPython = `
import base64,fcntl,hashlib,json,os,pwd,re,shutil,stat,subprocess,sys
scope,expected,action=sys.argv[1:4]
selected=sys.argv[4] if len(sys.argv)>4 else ''
managed_uid=-1;managed_gid=-1
probe_present=False
# This identity is synchronized with the single-file installer's embedded source.
probe_source_sha256='981fd3f6b7434849ea6b2fb751527c119e4ada90abef48f594f70b56e9ba1683'
class Rejected(Exception): pass
def reject(): raise Rejected()
def run(args):
    subprocess.run(args,check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=40)
def read(path):
    with open(path,encoding='utf-8') as f: return f.read()
def validate(path,allowed={0}):
    current=path
    while current!='/':
        if os.path.lexists(current) and os.path.islink(current): reject()
        parent=os.path.dirname(current)
        if parent==current: break
        current=parent
    if not os.path.lexists(path): return
    paths=[path]
    if os.path.isdir(path):
        for directory,dirs,files in os.walk(path,followlinks=False): paths.extend(os.path.join(directory,x) for x in dirs+files)
    if len(paths)>10000: reject()
    for candidate in paths:
        s=os.lstat(candidate)
        if not (stat.S_ISDIR(s.st_mode) or stat.S_ISREG(s.st_mode)) or s.st_uid not in allowed or s.st_mode&0o002 or os.path.ismount(candidate): reject()
        if s.st_mode&0o020 and not (scope=='msboost' and s.st_uid==managed_uid and s.st_gid==managed_gid): reject()
def fingerprint(path):
    records=[]
    paths=[path]
    if os.path.isdir(path):
        for directory,dirs,files in os.walk(path): paths.extend(os.path.join(directory,x) for x in dirs+files)
    for candidate in sorted(paths):
        s=os.lstat(candidate)
        digest=''
        if stat.S_ISREG(s.st_mode):
            if s.st_size>512*1024*1024: reject()
            h=hashlib.sha256()
            with open(candidate,'rb') as f:
                for block in iter(lambda:f.read(1024*1024),b''): h.update(block)
            digest=h.hexdigest()
        records.append([candidate,s.st_uid,s.st_gid,s.st_mode,digest])
    return records
def env(path):
    result={}
    for line in read(path).splitlines():
        if '=' in line:
            key,value=line.split('=',1)
            if key in result: reject()
            result[key]=value
    return result
def firewall(port,ufw,runtime,permanent,zone):
    if not re.fullmatch(r'[0-9]{1,5}',port) or not 1<=int(port)<=65535: reject()
    for name,value in [('ufw',ufw),('runtime',runtime),('permanent',permanent)]:
        if value not in ['0','1']: reject()
        if value=='1':
            if name=='ufw': firewalls.append(('ufw','',port))
            else:
                if not re.fullmatch(r'[A-Za-z0-9_.-]{1,80}',zone): reject()
                firewalls.append((name,zone,port))
def add(path,kind,allowed={0}):
    validate(path,allowed)
    if os.path.exists(path):
        targets.append(path);items.append({'path':path,'kind':kind});records.extend(fingerprint(path))
def check_unit_links(unit,path):
    target='multi-user.target' if unit=='msboost.service' else 'msboost.service'
    def directory(path):
        current=path
        while current!='/':
            if os.path.islink(current): reject()
            parent=os.path.dirname(current)
            if parent==current: break
            current=parent
        if os.path.lexists(path):
            info=os.lstat(path)
            if not stat.S_ISDIR(info.st_mode) or info.st_uid!=0 or info.st_mode&0o022: reject()
    for root in ['/etc/systemd/system','/run/systemd/system']:
        directory(root);directory(root+'/'+target+'.wants')
        runtime=root+'/'+unit
        if runtime!=path and os.path.lexists(runtime): reject()
        for parent,dirs,files in os.walk(root,followlinks=False):
            for name in files:
                link=parent+'/'+name
                if link==path: continue
                # disable also removes aliases whose filename differs from unit.
                if name!=unit and (not os.path.islink(link) or os.path.realpath(link)!=path): continue
                if link!=root+'/'+target+'.wants/'+unit: reject()
                info=os.lstat(link)
                if not os.path.islink(link) or (info.st_uid,info.st_gid)!=(0,0): reject()
                destination=os.readlink(link)
                if os.path.abspath(os.path.join(os.path.dirname(link),destination))!=path: reject()
                records.append([link,info.st_uid,info.st_gid,info.st_mode,hashlib.sha256(('unit-link:'+destination).encode()).hexdigest()])
                if root=='/run/systemd/system': runtime_units.append(unit)
def check_unit(unit,path,required=None):
    validate(path)
    if not os.path.isfile(path) or os.path.lexists(path+'.d'): reject()
    text=read(path)
    lines=[line.strip() for line in text.splitlines()]
    if any(line.endswith('\\') for line in lines): reject()
    values=dict(line.split('=',1) for line in required or [])
    credentials=[line.split('=',1)[1] for line in required or [] if line.startswith('LoadCredential=')]
    if unit=='msboost-tcp-probe.service':
        expected={'Unit':{'Description':'msboost TCP probe v2','After':'msboost.service','PartOf':'msboost.service','BindsTo':'msboost.service'},'Install':{'WantedBy':'msboost.service'},'Service':{'Type':'simple','User':'msboost','Group':'msboost','ExecStart':'/usr/bin/python3 -B /etc/msboost/relay_tcp_probe.py --config /etc/msboost/tcp-probe.json','Restart':'on-failure','RestartSec':'3s','SyslogIdentifier':'msboost-tcp-probe','UMask':'0027','NoNewPrivileges':'true','PrivateTmp':'true','ProtectHome':'true','ProtectSystem':'strict','ProtectKernelTunables':'true','ProtectKernelModules':'true','ProtectControlGroups':'true','RestrictSUIDSGID':'true','LockPersonality':'true','MemoryDenyWriteExecute':'true','RestrictAddressFamilies':'AF_INET','CapabilityBoundingSet':'','MemoryMax':'64M','TasksMax':'8','LimitNOFILE':'64'}}
    else:
        expected={'Unit':{'Description':values['Description'],'After':'network-online.target','Wants':'network-online.target'},'Install':{'WantedBy':'multi-user.target'}}
    if scope=='relay':
        expected['Service']={'Type':'simple','DynamicUser':'true','LoadCredential':credentials,'ExecStart':values['ExecStart'],'Restart':'on-failure','RestartSec':'3','NoNewPrivileges':'true','PrivateTmp':'true','ProtectSystem':'strict','ProtectHome':'true','RestrictAddressFamilies':'AF_INET AF_INET6 AF_UNIX'}
        if 'KillMode' in values: expected['Service']['KillMode']=values['KillMode']
    elif unit!='msboost-tcp-probe.service':
        expected['Service']={'Type':'simple','User':'msboost','Group':'msboost','WorkingDirectory':'/var/lib/msboost','ExecStart':values['ExecStart'],'Environment':'SAFE_PATHS=/etc/msboost/ruleset','Restart':'on-failure','RestartSec':'3s','SyslogIdentifier':'msboost','UMask':'0027','LimitNOFILE':'1048576','NoNewPrivileges':'true','PrivateTmp':'true','ProtectHome':'true','ProtectSystem':'strict','ProtectKernelTunables':'true','ProtectKernelModules':'true','ProtectControlGroups':'true','RestrictSUIDSGID':'true','LockPersonality':'true','MemoryDenyWriteExecute':'true','RestrictAddressFamilies':'AF_INET AF_INET6 AF_UNIX AF_NETLINK','CapabilityBoundingSet':'CAP_NET_BIND_SERVICE','AmbientCapabilities':'CAP_NET_BIND_SERVICE','ReadWritePaths':'/var/lib/msboost'}
        if probe_present and 'Wants=network-online.target msboost-tcp-probe.service' in lines:
            expected['Unit']['Wants']='network-online.target msboost-tcp-probe.service'
    # Compare the complete normalized template, not merely ExecStart. Added
    # PropagatesStopTo/Also/OnFailure/etc can affect unconfirmed third-party units.
    parsed={};section=None
    for line in lines:
        if not line or line.startswith(('#',';')): continue
        if line.startswith('[') and line.endswith(']'):
            section=line[1:-1]
            if section not in expected or section in parsed: reject()
            parsed[section]={}
        else:
            if section is None or '=' not in line: reject()
            key,value=(piece.strip() for piece in line.split('=',1))
            if key not in expected[section]: reject()
            if scope=='relay' and section=='Service' and key=='LoadCredential':
                parsed[section].setdefault(key,[]).append(value)
                if len(parsed[section][key])>len(credentials): reject()
            else:
                if key in parsed[section] or value!=expected[section][key]: reject()
                parsed[section][key]=value
    if parsed!=expected: reject()
    if scope=='msboost': check_unit_links(unit,path)
    fragment=subprocess.check_output(['systemctl','show',unit,'-p','FragmentPath','--value'],text=True,stderr=subprocess.DEVNULL,timeout=15).strip()
    if fragment!=path: reject()
    dropins=subprocess.check_output(['systemctl','show',unit,'-p','DropInPaths','--value'],text=True,stderr=subprocess.DEVNULL,timeout=15).strip()
    if dropins: reject()
    units.append(unit)
def check_probe_file(path,mode,gid):
    validate(path)
    if not os.path.isfile(path): reject()
    info=os.lstat(path)
    if info.st_uid!=0 or info.st_gid!=gid or stat.S_IMODE(info.st_mode)!=mode or info.st_nlink!=1: reject()
def check_probe():
    source='/etc/msboost/relay_tcp_probe.py';config='/etc/msboost/tcp-probe.json';unitpath='/etc/systemd/system/msboost-tcp-probe.service'
    check_probe_file(source,0o640,managed_gid);check_probe_file(config,0o640,managed_gid);check_probe_file(unitpath,0o644,0)
    if fingerprint(source)[0][4]!=probe_source_sha256: reject()
    def unique(pairs):
        document={}
        for key,value in pairs:
            if key in document: reject()
            document[key]=value
        return document
    if os.lstat(config).st_size>4096: reject()
    document=json.loads(read(config),object_pairs_hook=unique,parse_float=lambda value:reject(),parse_constant=lambda value:reject())
    expected={'listen_address':'127.0.0.1','listen_port':20424,'allowed_peer':'127.0.0.1','max_sessions':4,'max_connect_requests':3,'idle_timeout':3,'session_lifetime':15,'connect_timeout':2,'write_timeout':2,'max_line_bytes':4096}
    if type(document) is not dict or set(document)!=set(expected) or any(type(document[key]) is not type(value) or document[key]!=value for key,value in expected.items()): reject()
    check_unit('msboost-tcp-probe.service',unitpath)
    # Explicit resources appear in preview even though APP_DIR also contains them.
    for path in [source,config,unitpath]: add(path,'file')
try:
    if os.geteuid()!=0 or scope not in ['msboost','relay'] or action not in ['cleanup','cleanup-preview']: reject()
    if selected and (scope!='relay' or not re.fullmatch(r'[A-Za-z0-9_-]{1,100}',selected)): reject()
    locks=[]
    for path in ['/run/msboost-installer.lock','/run/msboost-customer-cleanup.lock']:
        validate(path)
        fd=os.open(path,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
        fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);locks.append(fd)
    targets=[];units=[];runtime_units=[];items=[];records=[];firewalls=[]
    if scope=='msboost':
        roots=['/etc/msboost','/var/lib/msboost','/usr/local/bin/msboost','/etc/systemd/system/msboost.service','/root/直连.json']
        probe_paths=['/etc/msboost/relay_tcp_probe.py','/etc/msboost/tcp-probe.json','/etc/systemd/system/msboost-tcp-probe.service']
        if os.path.lexists(probe_paths[2]+'.d'): reject()
        probe_present=any(os.path.lexists(path) for path in probe_paths)
        if probe_present and not all(os.path.lexists(path) for path in probe_paths): reject()
        if probe_present or any(os.path.lexists(x) for x in roots):
            validate('/etc/msboost/install.env')
            settings=env('/etc/msboost/install.env')
            if settings.get('MANAGED_BY')!='msboost-installer-v1': reject()
            account=pwd.getpwnam('msboost');uid=account.pw_uid
            if uid<=0 or account.pw_gid<=0: reject()
            managed_uid=uid;managed_gid=account.pw_gid
            permitted={
                '/etc/msboost':{'config.yaml','install.env','ruleset','relay_tcp_probe.py','tcp-probe.json'},
                '/etc/msboost/ruleset':{'msboost-direct.yaml'},
                '/var/lib/msboost':{'cache.db','cache.db-shm','cache.db-wal','ruleset'},
                '/var/lib/msboost/ruleset':{'msboost-filter.mrs'},
            }
            for directory,names in permitted.items():
                if os.path.isdir(directory) and not set(os.listdir(directory)).issubset(names): reject()
            if probe_present: check_probe()
            if os.path.exists('/root/直连.json'):
                client=json.loads(read('/root/直连.json'))
                profiles=client.get('profiles',[])
                if len(profiles)!=1 or profiles[0].get('user',{})!={'name':settings.get('USER_NAME'),'password':settings.get('USER_PASS')}: reject()
            check_unit('msboost.service','/etc/systemd/system/msboost.service',['Description=msboost service','ExecStart=/usr/local/bin/msboost -d /var/lib/msboost -f /etc/msboost/config.yaml','User=msboost'])
            for root in roots: add(root,'directory' if root in roots[:2] else 'file',{0,uid} if root in roots[:2] else {0})
            firewall(settings.get('FIREWALL_PORT',''),settings.get('FIREWALL_UFW_OWNED','0'),settings.get('FIREWALLD_RUNTIME_OWNED','0'),settings.get('FIREWALLD_PERMANENT_OWNED','0'),settings.get('FIREWALLD_ZONE',''))
    else:
        validate('/etc/msboost-free')
        candidates=set()
        if os.path.isdir('/etc/msboost-free'): candidates.update(os.listdir('/etc/msboost-free'))
        for name in os.listdir('/etc/systemd/system'):
            if name.startswith('msboost-free-') and name.endswith('.service'): candidates.add(name[len('msboost-free-'):-len('.service')])
        for task in sorted(candidates):
            if selected and task!=selected: continue
            if not re.fullmatch(r'[A-Za-z0-9_-]{1,100}',task): reject()
            conf='/etc/msboost-free/'+task
            unit='msboost-free-'+task+'.service';unitpath='/etc/systemd/system/'+unit
            validate(conf)
            if not os.path.isdir(conf): reject()
            # Older releases have no marker; the complete unit and config layout
            # below must still prove this project's installation convention.
            allowed={'config.json','guard.json','guard.py','managed-by','ufw-owned','firewalld-runtime-owned','firewalld-permanent-owned'}
            if not set(os.listdir(conf)).issubset(allowed) or not os.path.isfile(conf+'/config.json'): reject()
            if os.path.exists(conf+'/managed-by') and read(conf+'/managed-by').strip()!='msboost-free-v1': reject()
            validate(unitpath)
            unit_text=read(unitpath)
            legacy=re.search(r'^ExecStart=/usr/local/libexec/msboost-free/gost-([a-f0-9]{64}) -C (?:%d|\$\{CREDENTIALS_DIRECTORY\})/(config(?:\.json)?)$',unit_text,re.M)
            guarded=re.search(r'^ExecStart=/usr/bin/python3 \$\{CREDENTIALS_DIRECTORY\}/guard\.py /usr/local/libexec/msboost-free/gost-([a-f0-9]{64}) \$\{CREDENTIALS_DIRECTORY\}/config\.json \$\{CREDENTIALS_DIRECTORY\}/guard\.json$',unit_text,re.M)
            if guarded:
                if not os.path.isfile(conf+'/guard.json') or not os.path.isfile(conf+'/guard.py'): reject()
                required=['Description=MSBOOST customer TCP forwarding','DynamicUser=true','LoadCredential=config.json:'+conf+'/config.json','LoadCredential=guard.json:'+conf+'/guard.json','LoadCredential=guard.py:'+conf+'/guard.py',guarded.group(0),'KillMode=control-group']
                match=guarded
            elif legacy:
                if os.path.exists(conf+'/guard.json') or os.path.exists(conf+'/guard.py'): reject()
                required=['Description=MSBOOST customer TCP forwarding','DynamicUser=true','LoadCredential='+legacy.group(2)+':'+conf+'/config.json',legacy.group(0)]
                match=legacy
            else: reject()
            check_unit(unit,unitpath,required)
            binary='/usr/local/libexec/msboost-free/gost-'+match.group(1)
            validate(binary)
            if not os.path.isfile(binary): reject()
            document=json.loads(read(conf+'/config.json'))
            if len(document.get('services',[]))!=1 or document['services'][0].get('name')!='msboost-free': reject()
            if guarded:
                guard_config=json.loads(read(conf+'/guard.json'))
                if set(guard_config)!={'publicPort','backendPort'}: reject()
                port=guard_config['publicPort'];backend_port=guard_config['backendPort']
                if type(port) is not int or type(backend_port) is not int or not 1<=port<=65535 or not 1<=backend_port<=65535 or port==backend_port: reject()
                service=document['services'][0]
                if service.get('addr')!='127.0.0.1:'+str(backend_port) or service.get('admission')!='guard': reject()
                if document.get('admissions')!=[{'name':'guard','whitelist':True,'matchers':['127.0.0.1']}]: reject()
                port=str(port)
            else:
                port=str(document['services'][0].get('addr','')).lstrip(':')
            for mode in ['ufw','firewalld-runtime','firewalld-permanent']:
                path=conf+'/'+mode+'-owned'
                if os.path.exists(path):
                    fields=read(path).split()
                    if mode=='ufw':
                        if fields!=[port]: reject()
                        firewall(port,'1','0','0','')
                    else:
                        if len(fields)!=2 or fields[1]!=port: reject()
                        firewall(port,'0','1' if mode.endswith('runtime') else '0','1' if mode.endswith('permanent') else '0',fields[0])
            add(conf,'directory');add(unitpath,'file')
    for mode,zone,port in firewalls: items.append({'path':mode+':'+zone+':'+port+'/tcp','kind':'firewall'})
    digest=hashlib.sha256(json.dumps([scope,records,firewalls],sort_keys=True,separators=(',',':')).encode()).hexdigest()
    if selected: digest=hashlib.sha256((digest+':'+selected).encode()).hexdigest()
    if action=='cleanup':
        if expected!=digest:
            print('MSBOOST_ERROR_CODE=cleanup_changed',flush=True);sys.exit(1)
        # Recheck every path immediately before mutation; never follow symlinks.
        for unit in units:
            run(['systemctl','disable','--now',unit])
            if unit in runtime_units: run(['systemctl','disable','--runtime',unit])
        for mode,zone,port in firewalls:
            if mode=='ufw': run(['ufw','--force','delete','allow',port+'/tcp'])
            else:
                args=['firewall-cmd']+(['--permanent'] if mode=='permanent' else [])+['--zone='+zone,'--remove-port='+port+'/tcp']
                run(args)
        for path in targets:
            validate(path,{0,pwd.getpwnam('msboost').pw_uid} if scope=='msboost' and path in ['/etc/msboost','/var/lib/msboost'] else {0})
            if os.path.isdir(path): shutil.rmtree(path)
            elif os.path.exists(path): os.unlink(path)
        run(['systemctl','daemon-reload'])
    report={'scope':scope,'digest':digest,'items':items,'removed':action=='cleanup'}
    if selected: report['managedTaskId']=selected
    print('MSBOOST_CLEANUP='+base64.b64encode(json.dumps(report,separators=(',',':')).encode()).decode(),flush=True)
except Rejected:
    print('MSBOOST_ERROR_CODE=ownership_failed',flush=True);sys.exit(1)
except Exception:
    print('MSBOOST_ERROR_CODE='+('cleanup_failed' if action=='cleanup' else 'ownership_failed'),flush=True);sys.exit(1)
`
