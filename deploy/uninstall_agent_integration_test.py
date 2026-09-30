"""Destructive fixture-only test. Run exclusively inside the disposable CI image."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile

if os.getuid() != 0 or not Path('/.dockerenv').exists() or os.environ.get('MSBOOST_UNINSTALL_FIXTURE') != '1':
    raise SystemExit('Requires the explicitly enabled disposable container')

ROOT = Path('/var/lib/private/msboost-relay')
ID = 'a' * 40
SERVER = 'https://offline.invalid'
SCRIPT = '/src/deploy/uninstall-agent.sh'
UNIT = Path('/etc/systemd/system/msboost-relay.service')
ENV = Path('/etc/msboost-relay.env')
MANAGED = Path('/usr/local/libexec/msboost-agent')
BIN = Path('/usr/local/bin/msboost-agent')
BACKUPS = Path('/var/backups/msboost-agent')
MODE = Path('/run/fixture-stop-mode')
STATUS = Path('/run/fixture-active')
CALLS = Path('/run/fixture-calls')


def write(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(data)
    path.chmod(mode)


write(Path('/usr/local/bin/systemctl'), '''#!/usr/bin/env python3
import pathlib,sys
a=sys.argv[1:]
active=pathlib.Path('/run/fixture-active')
mode=pathlib.Path('/run/fixture-stop-mode').read_text()
with open('/run/fixture-calls','a') as f:f.write(' '.join(a)+'\\n')
if a[0]=='show':
 p=a[a.index('-p')+1]
 values={'FragmentPath':'/etc/systemd/system/msboost-relay.service','DropInPaths':'','KillMode':'control-group',
 'ActiveState':'active' if active.exists() else 'inactive','MainPID':'123' if active.exists() else '0',
 'ControlPID':'0','ControlGroup':'/system.slice/msboost-relay.service' if active.exists() or mode=='residual' else ''}
 print(values[p])
elif a[0]=='stop':
 if mode=='fail':sys.exit(1)
 active.unlink(missing_ok=True)
elif a[0]=='is-active':
 sys.exit(0 if a[-1]=='msboost-relay.service' and active.exists() else 3)
elif a[0] not in ('disable','daemon-reload'):sys.exit(1)
''', 0o755)


def reset(*, records=None, recovery=False, executor=False, extra=False, stop='normal'):
    # These fixed fixture paths exist only inside this disposable container.
    for path in [ROOT, BACKUPS, MANAGED, Path('/run/msboost-agent-install')]:
        if path.exists():
            shutil.rmtree(path)
    for path in [UNIT, ENV, BIN, Path('/etc/msboost-executor.env'), Path('/var/lib/msboost-relay')]:
        path.unlink(missing_ok=True)
    ROOT.mkdir(parents=True, mode=0o700)
    BACKUPS.mkdir(parents=True, mode=0o700)
    Path('/var/lib/msboost-relay').symlink_to('private/msboost-relay')
    write(UNIT, '[Unit]\nDescription=MSBOOST relay Agent\n[Service]\nDynamicUser=true\nEnvironmentFile=/etc/msboost-relay.env\nStateDirectory=msboost-relay\nExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/private/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy keep_last\n', 0o644)
    write(ENV, 'MSBOOST_SERVER_URL='+SERVER+'\n')
    write(BIN, 'fixture agent', 0o755)
    write(MANAGED/'managed-v1', 'MSBOOST_AGENT_MANAGED_V1\n')
    write(MANAGED/'gost-v3.3.0', 'fixture gost', 0o755)
    write(ROOT/'relay-token.json', json.dumps({'agentId': ID, 'token': 'fixture-private-only'}))
    write(ROOT/'relay-v2-state.json', json.dumps({'schema': 2, 'offlinePolicy': 'keep_last', 'agentId': ID, 'serverUrl': SERVER, 'recoveryRequired': recovery, 'records': records or {}}))
    for name in ['relay-health.json', 'relay-enrollment.json', 'traffic-v2-journal.json', '.relay-v2-1234', '.msboost-1234']:
        write(ROOT/name, '{}')
    # Unexpected historical backup content must be preserved, not gate uninstall.
    write(BACKUPS/'relay.ABCDEFGH'/'unrelated-historical-file', 'preserve me')
    if extra:
        write(ROOT/'foreign-file', 'not owned')
    if executor:
        write(Path('/etc/msboost-executor.env'), 'other role untouched')
    write(MODE, stop)
    write(STATUS, 'active')
    write(CALLS, '')


def run(*args, ok=True):
    result = subprocess.run(['bash', SCRIPT, '--agent-id', ID, '--server', SERVER, *args], capture_output=True, text=True)
    assert (result.returncode == 0) == ok, (result.returncode, result.stdout, result.stderr)
    assert 'fixture-private-only' not in result.stdout+result.stderr
    return result


for name, records, recovery, executor in [
    ('current-empty-state', {}, False, False),
    ('live-forwarding', {'r': {'state': 'ready', 'command': {'action': 'upsert'}}}, False, False),
    ('recovery-with-executor', {'r': {'state': 'unknown'}}, True, True),
]:
    reset(records=records, recovery=recovery, executor=executor)
    run('--acknowledge-stop')
    assert not ROOT.exists() and not UNIT.exists() and not ENV.exists() and not STATUS.exists()
    assert not Path('/var/lib/msboost-relay').is_symlink()
    assert (BACKUPS/'relay.ABCDEFGH'/'unrelated-historical-file').read_text() == 'preserve me'
    archives = list(BACKUPS.glob('uninstall-relay.*/relay.tar.gz'))
    assert len(archives) == 1 and archives[0].stat().st_mode & 0o077 == 0
    with tarfile.open(archives[0]) as archive:
        saved = json.load(archive.extractfile('var/lib/private/msboost-relay/relay-token.json'))
        assert saved['agentId'] == ID
    assert BIN.exists() == executor and MANAGED.exists() == executor
    if executor:
        assert Path('/etc/msboost-executor.env').read_text() == 'other role untouched'
    print('PASS', name, flush=True)

for name, extra, stop in [('foreign-files', True, 'normal'), ('stop-fails', False, 'fail'), ('children-remain', False, 'residual')]:
    reset(extra=extra, stop=stop)
    run('--acknowledge-stop', ok=False)
    assert ROOT.exists() and UNIT.exists() and ENV.exists() and BIN.exists()
    assert not list(BACKUPS.glob('uninstall-relay.*/relay.tar.gz'))
    print('PASS', name, flush=True)

reset()
run('--check')
assert STATUS.exists() and ROOT.exists() and not Path('/run/msboost-agent-install').exists()
assert not list(BACKUPS.glob('uninstall-relay.*'))
run(ok=False)
assert STATUS.exists()
run('--acknowledge-stop', '--agent-id', 'b'*40, ok=False)
assert STATUS.exists()
run('--acknowledge-stop', '--server', 'https://another.invalid', ok=False)
assert STATUS.exists()
print('PASS preview, mandatory confirmation and exact-target rejection', flush=True)

reset()
write(Path('/run/fixture-unrelated'), 'external target')
(ROOT/'relay-health.json').unlink()
(ROOT/'relay-health.json').symlink_to('/run/fixture-unrelated')
run('--acknowledge-stop', ok=False)
assert STATUS.exists() and Path('/run/fixture-unrelated').read_text() == 'external target'
print('PASS symlink escape rejection', flush=True)
