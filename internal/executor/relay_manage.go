package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Read-only check before a relay code is created: login, Debian 11-13, and
// the conditions the free relay installer relies on. Nothing is installed.
const relayCheckScript = `set -Eeuo pipefail
export LC_ALL=C
check_fail() { printf 'MSBOOST_ERROR_CODE=%s\nMSBOOST_ERROR_PHASE=preflight\n' "$1"; exit 1; }
[ -r /etc/os-release ] || check_fail unsupported_debian
. /etc/os-release
[ "${ID:-}" = debian ] || check_fail unsupported_debian
case "${VERSION_ID:-}" in 11|12|13) ;; *) check_fail unsupported_debian ;; esac
[ "$(id -u)" = 0 ] || check_fail unsupported_system
[ -d /run/systemd/system ] && command -v systemctl >/dev/null || check_fail unsupported_system
systemd_version=$(systemctl --version | awk 'NR==1 {print $2}')
[[ "$systemd_version" =~ ^[0-9]+$ ]] && [ "$systemd_version" -ge 247 ] || check_fail unsupported_system
command -v flock >/dev/null || check_fail unsupported_system
case "$(uname -m)" in x86_64|aarch64|arm64) ;; *) check_fail unsupported_system ;; esac
if ! command -v python3 >/dev/null || ! command -v curl >/dev/null || ! command -v tar >/dev/null; then
  command -v apt-get >/dev/null || check_fail missing_dependency
fi
printf 'MSBOOST_READY=1\nMSBOOST_OS=Debian %s\n' "$VERSION_ID"
`

// relayLayoutPython recognizes only this project's free relay layout and never
// follows symbolic links. Unknown directories are skipped, not reported.
const relayLayoutPython = `
import base64,json,os,re,stat,subprocess,sys,time
ROOT='/etc/msboost-free'
class Skip(Exception): pass
def regular(path,limit=65536):
    value=os.lstat(path)
    if not stat.S_ISREG(value.st_mode) or value.st_uid!=0 or value.st_size>limit: raise Skip()
    with open(path,'rb') as stream: return stream.read(limit+1)
def managed(name):
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,100}',name): raise Skip()
    conf=ROOT+'/'+name
    unitpath='/etc/systemd/system/msboost-free-'+name+'.service'
    value=os.lstat(conf)
    if not stat.S_ISDIR(value.st_mode) or value.st_uid!=0: raise Skip()
    unit=regular(unitpath).decode('utf-8','replace')
    if 'Description=MSBOOST customer TCP forwarding' not in unit: raise Skip()
    config=json.loads(regular(conf+'/config.json'))
    services=config.get('services',[])
    if len(services)!=1 or services[0].get('name')!='msboost-free': raise Skip()
    if os.path.lexists(conf+'/guard.json'):
        port=json.loads(regular(conf+'/guard.json')).get('publicPort')
    else:
        port=int(str(services[0].get('addr','')).rsplit(':',1)[-1])
    if type(port) is not int or not 1<=port<=65535: raise Skip()
    return conf,unitpath,config,port
def properties(unit):
    out=subprocess.run(['systemctl','show',unit,'-p','ActiveState','-p','UnitFileState'],capture_output=True,text=True,timeout=15).stdout
    return dict(line.split('=',1) for line in out.splitlines() if '=' in line)
def sockets(port,state):
    count=0
    for name in ('tcp','tcp6'):
        try:
            with open('/proc/net/'+name) as stream:
                for line in list(stream)[1:]:
                    fields=line.split()
                    if fields[3]==state and int(fields[1].rsplit(':',1)[1],16)==port: count+=1
        except (OSError,ValueError,IndexError): pass
    return count
def rate(config):
    limiter=config['services'][0].get('limiter')
    for item in config.get('limiters',[]):
        if limiter and item.get('name')==limiter:
            match=re.fullmatch(r'\$ ([0-9]+)B ([0-9]+)B',(item.get('limits') or [''])[0])
            if match: return round(int(match.group(1))/125000)
    return 0
`

const relayInventoryScript = `set -Eeuo pipefail
export LC_ALL=C
[ "$(id -u)" = 0 ]
. /etc/os-release
printf 'MSBOOST_OS=Debian %s\n' "${VERSION_ID:-}"
python3 - <<'PY'
` + relayLayoutPython + `
entries=[]
names=[]
if os.path.isdir(ROOT) and not os.path.islink(ROOT): names=sorted(os.listdir(ROOT))
for name in names:
    try:
        conf,unitpath,config,port=managed(name)
        unit='msboost-free-'+name+'.service'
        props=properties(unit)
        active=props.get('ActiveState')=='active'
        paused=os.path.lexists(conf+'/paused')
        record=None
        if os.path.lexists(conf+'/meta.json'):
            try:
                record=json.loads(regular(conf+'/meta.json',16384).decode('utf-8'))
                if not isinstance(record,dict): record=None
            except (Skip,ValueError,UnicodeDecodeError): record=None
        target=''
        nodes=config['services'][0].get('forwarder',{}).get('nodes',[])
        if nodes: target=str(nodes[0].get('addr',''))
        entries.append({'id':name,'role':{'r':'relay','f':'front','p':'route'}.get(name[-1],'relay'),'port':port,'rateMbps':rate(config),'target':target,
            'state':'running' if active else ('paused' if paused else 'stopped'),'connections':sockets(port,'01') if active else 0,
            'createdAt':int(os.lstat(unitpath).st_mtime*1000),'record':record})
    except (Skip,OSError,ValueError,KeyError,TypeError,IndexError,subprocess.SubprocessError):
        continue
print('MSBOOST_INVENTORY='+base64.b64encode(json.dumps(entries,ensure_ascii=False,separators=(',',':')).encode()).decode(),flush=True)
PY
`

// Every action holds the same lock as installs and cleanups, touches only the
// listed managed relays, and reports a result for each one.
const relayManageScript = `
[ "$(id -u)" = 0 ]
[ ! -L /run/msboost-customer-cleanup.lock ]
exec 8>/run/msboost-customer-cleanup.lock
flock -w 30 8
python3 - "$manage_action" "$manage_rate" "$manage_items" <<'PY'
` + relayLayoutPython + `
action,mbps,items=sys.argv[1],int(sys.argv[2] or 0),sys.argv[3].split()
def run(args): return subprocess.run(args,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=60).returncode
def write_private(path,data):
    temporary=path+'.msboost-new'
    if os.path.lexists(temporary): os.unlink(temporary)
    descriptor=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(descriptor,'w',encoding='utf-8') as stream: stream.write(data)
    os.replace(temporary,path)
def started(unit,port):
    for _ in range(30):
        time.sleep(0.5)
        if properties(unit).get('ActiveState')=='active' and sockets(port,'0A')>0: return True
    return False
results=[]
for name in items:
    unit='msboost-free-'+name+'.service'
    try:
        conf,unitpath,config,port=managed(name)
    except (Skip,OSError,ValueError,KeyError,TypeError,IndexError,subprocess.SubprocessError):
        results.append({'id':name,'ok':False,'error':'not_found'});continue
    try:
        paused=os.path.lexists(conf+'/paused')
        if action=='rate':
            limit='$ %dB %dB'%(mbps*125000,mbps*125000)
            config['services'][0]['limiter']='per-port'
            config['limiters']=[{'name':'per-port','limits':[limit]}]
            write_private(conf+'/config.json',json.dumps(config))
            if os.path.lexists(conf+'/meta.json'):
                try:
                    record=json.loads(regular(conf+'/meta.json',16384).decode('utf-8'))
                    if isinstance(record,dict):
                        record['rateMbps']=mbps
                        write_private(conf+'/meta.json',json.dumps(record,ensure_ascii=False))
                except (Skip,ValueError,UnicodeDecodeError): pass
            if properties(unit).get('ActiveState')=='active':
                run(['systemctl','restart',unit])
                if not started(unit,port): raise RuntimeError('service_failed')
        elif action=='pause':
            run(['systemctl','disable','--now',unit])
            if properties(unit).get('ActiveState')=='active': raise RuntimeError('service_failed')
            if not paused: write_private(conf+'/paused','')
        elif action=='resume' or action=='restart':
            if action=='restart' and paused: raise RuntimeError('paused')
            if paused: os.unlink(conf+'/paused')
            run(['systemctl','reset-failed',unit])
            if run(['systemctl','enable',unit])!=0: raise RuntimeError('service_failed')
            run(['systemctl','restart',unit])
            if not started(unit,port): raise RuntimeError('service_failed')
        elif action=='delete':
            run(['systemctl','disable','--now',unit])
            if properties(unit).get('ActiveState')=='active': raise RuntimeError('service_failed')
            for mode in ('ufw','firewalld-runtime','firewalld-permanent'):
                path=conf+'/'+mode+'-owned'
                if not os.path.lexists(path): continue
                fields=regular(path,256).decode().split()
                if mode=='ufw' and len(fields)==1 and fields[0].isdigit():
                    run(['ufw','--force','delete','allow',fields[0]+'/tcp'])
                elif mode!='ufw' and len(fields)==2 and re.fullmatch(r'[A-Za-z0-9_.-]+',fields[0]) and fields[1].isdigit():
                    run(['firewall-cmd']+(['--permanent'] if mode.endswith('permanent') else [])+['--zone='+fields[0],'--remove-port='+fields[1]+'/tcp'])
            os.unlink(unitpath)
            for entry in os.listdir(conf): os.unlink(conf+'/'+entry)
            os.rmdir(conf)
            run(['systemctl','daemon-reload'])
        results.append({'id':name,'ok':True})
    except RuntimeError as error:
        results.append({'id':name,'ok':False,'error':str(error)})
    except (Skip,OSError,ValueError,KeyError,TypeError,IndexError,subprocess.SubprocessError):
        results.append({'id':name,'ok':False,'error':'failed'})
print('MSBOOST_MANAGE='+base64.b64encode(json.dumps(results,separators=(',',':')).encode()).decode(),flush=True)
PY
`

func (e *Engine) relayCheck(ctx context.Context, job Job, r Result) Result {
	out, err := e.Remote.Run(ctx, job.Request.SSH, relayCheckScript)
	if err != nil || marker(out, "MSBOOST_READY") != "1" {
		if err == nil {
			err = diagnosticError("execution_failed")
		}
		return failRemote(r, "preflight", out, err)
	}
	// The panel requires the version on success; never report one without it.
	if r.OS = cleanOS(marker(out, "MSBOOST_OS")); r.OS == "" {
		return failRemote(r, "preflight", out, diagnosticError("unsupported_debian"))
	}
	r.State, r.Phase = "succeeded", "complete"
	r.Message = "中转服务器检查通过"
	return r
}

func (e *Engine) relayInventory(ctx context.Context, job Job, r Result) Result {
	out, err := e.Remote.Run(ctx, job.Request.SSH, relayInventoryScript)
	if err != nil {
		return failRemote(r, "relay", out, err)
	}
	entries, ok := decodeMarker[[]RelayEntry](out, "MSBOOST_INVENTORY")
	if !ok {
		return failRemote(r, "relay", out, diagnosticError("execution_failed"))
	}
	r.State, r.Phase = "succeeded", "complete"
	r.OS = cleanOS(marker(out, "MSBOOST_OS"))
	r.Inventory = cleanInventory(entries, job.Request.ReportVerified)
	r.Message = "已读取中转列表"
	return r
}

func (e *Engine) relayManage(ctx context.Context, job Job, r Result) Result {
	m := job.Request.Manage
	script := "set -Eeuo pipefail\numask 077\nexport LC_ALL=C\n" + encodedAssignment("manage_action", m.Action) + encodedAssignment("manage_rate", strconv.Itoa(m.RateMbps)) + encodedAssignment("manage_items", strings.Join(m.Items, " ")) + relayManageScript
	out, err := e.Remote.Run(ctx, job.Request.SSH, script)
	if err != nil {
		return failRemote(r, "relay", out, err)
	}
	results, ok := decodeMarker[[]RelayManageResult](out, "MSBOOST_MANAGE")
	if !ok {
		return failRemote(r, "relay", out, diagnosticError("execution_failed"))
	}
	// Report exactly one result per requested item, in request order.
	byID := map[string]RelayManageResult{}
	for _, item := range results {
		byID[item.ID] = item
	}
	r.Managed = make([]RelayManageResult, 0, len(m.Items))
	for _, id := range m.Items {
		item, found := byID[id]
		if !found {
			item = RelayManageResult{ID: id, Error: "failed"}
		}
		item.ID = id
		switch item.Error {
		case "", "not_found", "service_failed", "paused", "failed":
		default:
			item.Error = "failed"
		}
		if item.OK {
			item.Error = ""
		} else if item.Error == "" {
			item.Error = "failed"
		}
		r.Managed = append(r.Managed, item)
	}
	r.State, r.Phase = "succeeded", "complete"
	r.Message = "中转管理操作已执行"
	return r
}

func decodeMarker[T any](out []byte, key string) (T, bool) {
	var value T
	raw, err := base64.StdEncoding.DecodeString(marker(out, key))
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return value, false
	}
	return value, true
}

func cleanOS(value string) string {
	switch value {
	case "Debian 11", "Debian 12", "Debian 13":
		return value
	}
	return ""
}

// The server is the owner's, but its files are still untrusted input: keep
// only well-formed fields with bounded sizes before they reach the panel.
func cleanInventory(entries []RelayEntry, reportVerified bool) []RelayEntry {
	out := make([]RelayEntry, 0, len(entries))
	for _, entry := range entries {
		if !managedRelayID.MatchString(entry.ID) || entry.Port < 1 || entry.Port > 65535 || len(out) >= 500 {
			continue
		}
		switch entry.Role {
		case "relay", "front", "route":
		default:
			entry.Role = "relay"
		}
		switch entry.State {
		case "running", "paused", "stopped":
		default:
			entry.State = "stopped"
		}
		if entry.RateMbps < 0 || entry.RateMbps > 10000 {
			entry.RateMbps = 0
		}
		if entry.Connections < 0 || entry.Connections > 1000000 {
			entry.Connections = 0
		}
		entry.Target = boundedText(entry.Target, 300)
		if entry.Record != nil {
			record := *entry.Record
			if record.Version != 1 || (record.Source != "manual" && record.Source != "code") {
				entry.Record = nil
			} else {
				record.User = boundedText(record.User, 254)
				record.Panel = boundedText(record.Panel, 200)
				record.Target = boundedText(record.Target, 300)
				record.Remark = boundedText(record.Remark, 60)
				if !codeTailPattern.MatchString(record.CodeTail) {
					record.CodeTail = ""
				}
				if record.RateMbps < 0 || record.RateMbps > MaxRelayRateMbps {
					record.RateMbps = 0
				}
				if at, err := time.Parse(time.RFC3339, record.CreatedAt); err == nil {
					entry.CreatedAt = at.UnixMilli()
				}
				record.CreatedAt = boundedText(record.CreatedAt, 40)
				record.Verified = cleanVerified(record.Verified, reportVerified)
				entry.Record = &record
			}
		}
		out = append(out, entry)
	}
	return out
}

func boundedText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	if runes := []rune(value); len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}
