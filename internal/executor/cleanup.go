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
		encodedAssignment("cleanup_scope", opts.Scope) + encodedAssignment("cleanup_digest", opts.Digest) + encodedAssignment("cleanup_action", job.Request.Kind) +
		"python3 - \"$cleanup_scope\" \"$cleanup_digest\" \"$cleanup_action\" <<'MSBOOST_CLEANUP_PY'\n" + cleanupPython + "\nMSBOOST_CLEANUP_PY\n"
	out, err := e.Remote.Run(ctx, job.Request.SSH, script)
	if err != nil {
		return failRemote(result, "cleanup", out, err)
	}
	raw, err := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CLEANUP"))
	var report CleanupReport
	if err != nil || json.Unmarshal(raw, &report) != nil || !ValidCleanupReport(&report, opts.Scope, job.Request.Kind == "cleanup") {
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
	for _, item := range report.Items {
		if !validCleanupItem(scope, item) {
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
	for _, path := range []string{"/etc/msboost", "/var/lib/msboost", "/usr/local/bin/msboost", "/etc/systemd/system/msboost.service", "/root/直连.json"} {
		if item.Path == path {
			return true
		}
	}
	return false
}

const cleanupPython = `
import base64,fcntl,hashlib,json,os,pwd,re,shutil,stat,subprocess,sys
scope,expected,action=sys.argv[1:]
managed_uid=-1;managed_gid=-1
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
        current=os.path.dirname(current)
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
def check_unit(unit,path,required):
    validate(path)
    if not os.path.isfile(path) or os.path.lexists(path+'.d'): reject()
    text=read(path)
    lines=[line.strip() for line in text.splitlines()]
    if any(line.endswith('\\') for line in lines): reject()
    values=dict(line.split('=',1) for line in required)
    expected={'Unit':{'Description':values['Description'],'After':'network-online.target','Wants':'network-online.target'},'Install':{'WantedBy':'multi-user.target'}}
    if scope=='relay':
        expected['Service']={'Type':'simple','DynamicUser':'true','LoadCredential':values['LoadCredential'],'ExecStart':values['ExecStart'],'Restart':'on-failure','RestartSec':'3','NoNewPrivileges':'true','PrivateTmp':'true','ProtectSystem':'strict','ProtectHome':'true','RestrictAddressFamilies':'AF_INET AF_INET6 AF_UNIX'}
    else:
        expected['Service']={'Type':'simple','User':'msboost','Group':'msboost','WorkingDirectory':'/var/lib/msboost','ExecStart':values['ExecStart'],'Environment':'SAFE_PATHS=/etc/msboost/ruleset','Restart':'on-failure','RestartSec':'3s','SyslogIdentifier':'msboost','UMask':'0027','LimitNOFILE':'1048576','NoNewPrivileges':'true','PrivateTmp':'true','ProtectHome':'true','ProtectSystem':'strict','ProtectKernelTunables':'true','ProtectKernelModules':'true','ProtectControlGroups':'true','RestrictSUIDSGID':'true','LockPersonality':'true','MemoryDenyWriteExecute':'true','RestrictAddressFamilies':'AF_INET AF_INET6 AF_UNIX AF_NETLINK','CapabilityBoundingSet':'CAP_NET_BIND_SERVICE','AmbientCapabilities':'CAP_NET_BIND_SERVICE','ReadWritePaths':'/var/lib/msboost'}
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
            if key in parsed[section] or key not in expected[section] or value!=expected[section][key]: reject()
            parsed[section][key]=value
    if parsed!=expected: reject()
    fragment=subprocess.check_output(['systemctl','show',unit,'-p','FragmentPath','--value'],text=True,stderr=subprocess.DEVNULL,timeout=15).strip()
    if fragment!=path: reject()
    dropins=subprocess.check_output(['systemctl','show',unit,'-p','DropInPaths','--value'],text=True,stderr=subprocess.DEVNULL,timeout=15).strip()
    if dropins: reject()
    units.append(unit)
try:
    if os.geteuid()!=0 or scope not in ['msboost','relay'] or action not in ['cleanup','cleanup-preview']: reject()
    locks=[]
    for path in ['/run/msboost-installer.lock','/run/msboost-customer-cleanup.lock']:
        validate(path)
        fd=os.open(path,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
        fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);locks.append(fd)
    targets=[];units=[];items=[];records=[];firewalls=[]
    if scope=='msboost':
        roots=['/etc/msboost','/var/lib/msboost','/usr/local/bin/msboost','/etc/systemd/system/msboost.service','/root/直连.json']
        if any(os.path.lexists(x) for x in roots):
            validate('/etc/msboost/install.env')
            settings=env('/etc/msboost/install.env')
            if settings.get('MANAGED_BY')!='msboost-installer-v1': reject()
            account=pwd.getpwnam('msboost');uid=account.pw_uid
            managed_uid=uid;managed_gid=account.pw_gid
            permitted={
                '/etc/msboost':{'config.yaml','install.env','ruleset'},
                '/etc/msboost/ruleset':{'msboost-direct.yaml'},
                '/var/lib/msboost':{'cache.db','cache.db-shm','cache.db-wal','ruleset'},
                '/var/lib/msboost/ruleset':{'msboost-filter.mrs'},
            }
            for directory,names in permitted.items():
                if os.path.isdir(directory) and not set(os.listdir(directory)).issubset(names): reject()
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
            if not re.fullmatch(r'[A-Za-z0-9_-]{1,100}',task): reject()
            conf='/etc/msboost-free/'+task
            unit='msboost-free-'+task+'.service';unitpath='/etc/systemd/system/'+unit
            validate(conf)
            if not os.path.isdir(conf): reject()
            # Older releases have no marker; the complete unit and config layout
            # below must still prove this project's installation convention.
            allowed={'config.json','managed-by','ufw-owned','firewalld-runtime-owned','firewalld-permanent-owned'}
            if not set(os.listdir(conf)).issubset(allowed) or not os.path.isfile(conf+'/config.json'): reject()
            if os.path.exists(conf+'/managed-by') and read(conf+'/managed-by').strip()!='msboost-free-v1': reject()
            validate(unitpath)
            match=re.search(r'^ExecStart=/usr/local/libexec/msboost-free/gost-([a-f0-9]{64}) -C %d/config$',read(unitpath),re.M)
            if not match: reject()
            check_unit(unit,unitpath,['Description=MSBOOST customer TCP forwarding','DynamicUser=true','LoadCredential=config:'+conf+'/config.json',match.group(0)])
            binary='/usr/local/libexec/msboost-free/gost-'+match.group(1)
            validate(binary)
            if not os.path.isfile(binary): reject()
            document=json.loads(read(conf+'/config.json'))
            if len(document.get('services',[]))!=1 or document['services'][0].get('name')!='msboost-free': reject()
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
    if action=='cleanup':
        if expected!=digest:
            print('MSBOOST_ERROR_CODE=cleanup_changed',flush=True);sys.exit(1)
        # Recheck every path immediately before mutation; never follow symlinks.
        for unit in units: run(['systemctl','disable','--now',unit])
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
    print('MSBOOST_CLEANUP='+base64.b64encode(json.dumps(report,separators=(',',':')).encode()).decode(),flush=True)
except Rejected:
    print('MSBOOST_ERROR_CODE=ownership_failed',flush=True);sys.exit(1)
except Exception:
    print('MSBOOST_ERROR_CODE='+('cleanup_failed' if action=='cleanup' else 'ownership_failed'),flush=True);sys.exit(1)
`
