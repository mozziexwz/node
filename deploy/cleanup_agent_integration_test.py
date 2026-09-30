"""One-role and incomplete-install cleanup, exclusively in the CI container."""
import os
import fcntl
from pathlib import Path
import pty
import runpy
import select
import signal
import subprocess
import time

if os.getuid() != 0 or not Path('/.dockerenv').exists() or os.environ.get('MSBOOST_UNINSTALL_FIXTURE') != '1':
    raise SystemExit('Requires the explicitly enabled disposable container')

# Reuse the existing exact-path fixture and retain all its normal-uninstall tests.
fixture = runpy.run_path('/src/deploy/uninstall_agent_integration_test.py')
reset, write = fixture['reset'], fixture['write']
ROOT, UNIT, ENV, BIN = [fixture[name] for name in ('ROOT', 'UNIT', 'ENV', 'BIN')]
SCRIPT = '/src/deploy/cleanup-agent.sh'
executor_unit = Path('/etc/systemd/system/msboost-executor.service')

def setup(mode='normal', firewall=False):
    reset(executor=True, firewall=firewall)
    Path('/run/fixture-active').unlink(missing_ok=True)
    write(executor_unit, '[Unit]\nDescription=MSBOOST executor Agent\n[Service]\nDynamicUser=true\nEnvironmentFile=/etc/msboost-executor.env\nExecStart=/usr/local/bin/msboost-agent --capability executor --state-dir /var/lib/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0\n', 0o644)
    for role in ('relay','executor'):
        write(Path('/run/fixture-role-'+role), 'active')
    write(Path('/usr/local/bin/systemctl'), '''#!/usr/bin/env python3
import pathlib,sys
a=sys.argv[1:]; role='executor' if 'msboost-executor.service' in a else 'relay'
active=pathlib.Path('/run/fixture-role-'+role); unit='/etc/systemd/system/msboost-'+role+'.service'
mode=pathlib.Path('/run/fixture-stop-mode').read_text()
with open('/run/fixture-calls','a') as f:f.write(' '.join(a)+'\\n')
if a[0]=='show':
 p=a[a.index('-p')+1]; exists=pathlib.Path(unit).exists()
 values={'ActiveState':'active' if active.exists() else 'inactive', 'FragmentPath':unit if exists else '', 'DropInPaths':'', 'KillMode':'control-group', 'MainPID':'123' if active.exists() else '0', 'ControlPID':'0', 'ControlGroup':'/system.slice/msboost-'+role+'.service' if active.exists() or mode=='residual' else ''}
 print(values[p])
elif a[0]=='stop':
 if mode=='fail':sys.exit(1)
 active.unlink(missing_ok=True)
elif a[0] not in ('disable','daemon-reload'):sys.exit(1)
''', 0o755)
    write(Path('/run/fixture-stop-mode'), mode)

def run(role, *, cleanup=False, confirm=True, ok=True):
    args=['bash',SCRIPT,'--capability',role,'--cleanup' if cleanup else '--check']
    if not cleanup:
        result=subprocess.run(args,capture_output=True,text=True)
        assert (result.returncode==0)==ok,(result.stdout,result.stderr)
        return
    pid,fd=pty.fork()
    if pid==0: os.execvp(args[0],args)
    output=b'';sent=False;ended=0;status=0;deadline=time.monotonic()+20
    try:
        while time.monotonic()<deadline:
            if select.select([fd],[],[],0.1)[0]:
                try: data=os.read(fd,65536)
                except OSError: break
                if not data:break
                output+=data
                if not sent and b'CLEAN_MSBOOST_' in output:
                    os.write(fd,((('CLEAN_MSBOOST_'+role.upper()) if confirm else 'CANCEL')+'\n'+os.uname().nodename+'\n').encode());sent=True
            ended,status=os.waitpid(pid,os.WNOHANG)
            if ended:break
        else:
            os.kill(pid,signal.SIGKILL);raise AssertionError('cleanup exceeded fixture timeout')
        if not ended: _,status=os.waitpid(pid,0)
        assert (os.waitstatus_to_exitcode(status)==0)==ok,output.decode(errors='replace')
        assert b'fixture-private-only' not in output
    finally:os.close(fd)

setup()
run('executor')
assert executor_unit.exists() and ROOT.exists()
run('executor',cleanup=True)
assert not executor_unit.exists() and not Path('/etc/msboost-executor.env').exists()
assert ROOT.exists() and UNIT.exists() and ENV.exists() and BIN.exists()
print('PASS independent executor cleanup preserves relay and shared programs',flush=True)

for missing in ('identity','environment','unit'):
    setup()
    if missing=='identity': (ROOT/'relay-token.json').unlink();(ROOT/'relay-v2-state.json').write_text('corrupt identity')
    elif missing=='environment':ENV.unlink()
    else:UNIT.unlink();Path('/run/fixture-role-relay').unlink()
    run('relay')
    run('relay',cleanup=True)
    assert not ROOT.exists() and not UNIT.exists() and not ENV.exists()
    assert executor_unit.exists() and BIN.exists()
    print('PASS incomplete managed '+missing+' cleanup with local double confirmation',flush=True)

for bad in ('cancel','foreign','symlink','stop-fails','residual','hook'):
    setup('fail' if bad=='stop-fails' else 'residual' if bad=='residual' else 'normal')
    if bad=='foreign':write(ROOT/'not-owned.txt','preserve me')
    elif bad=='symlink':(ROOT/'relay-health.json').unlink();(ROOT/'relay-health.json').symlink_to('/run/fixture-unrelated')
    elif bad=='hook':UNIT.write_text(UNIT.read_text()+'ExecStop=/bin/false\n')
    run('relay',cleanup=True,confirm=bad!='cancel',ok=False)
    assert ROOT.exists() and UNIT.exists() and ENV.exists() and executor_unit.exists() and BIN.exists()
    print('PASS cleanup rejects '+bad+' without deleting components',flush=True)

setup()
lockdir=Path('/run/msboost-agent-install');lockdir.mkdir(mode=0o700,exist_ok=True)
lock=lockdir/'install.lock'
with lock.open('w') as stream:
    lock.chmod(0o600)
    fcntl.flock(stream,fcntl.LOCK_EX|fcntl.LOCK_NB)
    run('executor',cleanup=True,ok=False)
assert executor_unit.exists() and ROOT.exists()
print('PASS cleanup shares the installer lock',flush=True)

setup(firewall=True)
run('relay',cleanup=True)
assert not Path('/etc/systemd/system/msboost-relay-firewall.service').exists()
assert not Path('/usr/local/libexec/msboost-agent/relay-firewall').exists()
assert executor_unit.exists() and BIN.exists()
print('PASS incomplete-role cleanup includes the owned firewall companion, not executor',flush=True)

setup(firewall=True)
write(Path('/run/fixture-firewall-fail'),'fail')
run('relay',cleanup=True,ok=False)
assert ROOT.exists() and UNIT.exists() and executor_unit.exists() and BIN.exists()
assert Path('/etc/systemd/system/msboost-relay-firewall.service').exists()
print('PASS failed firewall cleanup does not remove remaining role files',flush=True)

for unsafe in (Path('/etc/systemd/system/msboost-relay-firewall.service'), Path('/usr/local/libexec/msboost-agent/relay-firewall')):
    setup(firewall=True)
    unsafe.chmod(0o777)
    run('relay',cleanup=True,ok=False)
    assert ROOT.exists() and UNIT.exists() and executor_unit.exists() and BIN.exists()
    assert Path('/run/fixture-role-relay').exists()
print('PASS cleanup refuses writable privileged firewall components',flush=True)
