#!/usr/bin/env python3
"""Loopback-only, CPU-affined end-to-end Veil comparison; Python is orchestration.
The actual load and TCP backend are the existing Go benchpeer, not Python.
"""
import argparse
import base64
import hashlib
import itertools
import json
import os
from pathlib import Path
import random
import select
import socket
import statistics
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def line(proc):
    if not select.select([proc.stdout], [], [], 90)[0]:
        raise TimeoutError('load timed out')
    raw = proc.stdout.readline()
    if not raw:
        raise RuntimeError('load failed, see process logs')
    return json.loads(raw)


def wait(p, proc):
    for _ in range(300):
        if proc.poll() is not None:
            raise RuntimeError('proxy exited, see process logs')
        try:
            with socket.create_connection(('127.0.0.1', p), .05):
                return
        except OSError:
            time.sleep(.01)
    raise TimeoutError('proxy readiness')


def counters(pid):
    stat = Path(f'/proc/{pid}/stat').read_text().split(') ', 1)[1].split()
    status = Path(f'/proc/{pid}/status').read_text().splitlines()
    return {'cpu_seconds': (int(stat[11])+int(stat[12]))/os.sysconf('SC_CLK_TCK'),
            'peak_rss_kib': int(next(s for s in status if s.startswith('VmHWM:')).split()[1])}


class Perf:
    def __init__(self, pid, folder):
        folder.mkdir()
        self.ctl, self.ack, self.output = [folder/n for n in ['ctl', 'ack', 'perf.csv']]
        os.mkfifo(self.ctl)
        os.mkfifo(self.ack)
        self.cf = os.open(self.ctl, os.O_RDWR | os.O_NONBLOCK)
        self.af = os.open(self.ack, os.O_RDWR | os.O_NONBLOCK)
        self.log = (folder/'log').open('w')
        self.proc = subprocess.Popen(['sudo', '-n', 'perf', 'stat', '-p', str(pid), '--delay=-1',
            f'--control=fifo:{self.ctl},{self.ack}', '-x,', '-e', 'task-clock,cycles,instructions',
            '-o', str(self.output)], stdout=self.log, stderr=self.log)

    def command(self, command):
        os.write(self.cf, (command+'\n').encode())
        if not select.select([self.af], [], [], 10)[0] or b'ack' not in os.read(self.af, 4096):
            raise RuntimeError('perf failed; inspect log or use --no-perf')

    def close(self):
        if self.proc.poll() is None:
            subprocess.run(['sudo', '-n', 'kill', '-INT', str(self.proc.pid)], check=True)
            self.proc.wait(timeout=10)
        os.close(self.cf)
        os.close(self.af)
        self.ctl.unlink()
        self.ack.unlink()
        self.log.close()

    def result(self):
        result = {}
        for row in self.output.read_text().splitlines():
            parts = row.split(',')
            if len(parts)>2:
                try:
                    result[parts[2]] = float(parts[0])
                except ValueError:
                    pass
        if result.get('task-clock', 0) <= 0:
            raise RuntimeError('missing perf task-clock')
        return result


def trial(args, pair, mode, concurrency, folder, cert, key):
    folder.mkdir()
    bp, sp, cp = [port() for _ in range(3)]
    binaries = {'go': args.go, 'rust': args.rust, 'openssl': args.openssl}
    client_name, server_name = pair.split('-')
    base = {'secret': base64.urlsafe_b64encode(os.urandom(32)).rstrip(b'=').decode(),
            'max_connections': 64, 'max_idle': 8, 'idle_seconds': 120, 'pool_seconds': 60,
            'traffic': {'version': 1, 'quantum_bytes': {'min':8192,'max':32768},
                        'startup_bytes': {'min':512,'max':2048}, 'startup_writes': {'min':2,'max':6},
                        'padding_limit': {'min':0,'max':0}, 'padding_budget': {'min':0,'max':0},
                        'control_padding_limit': {'min':0,'max':0}, 'credit_blocks': {'min':16,'max':64}}}
    tls = {'mode':'tls','server_name':'cover.test','record_padding':False}
    sc = base | {'role':'server','listen':f'127.0.0.1:{sp}', 'tls':tls|{'certificate':str(cert),'private_key_file':str(key)}}
    cc = base | {'role':'client','listen':f'127.0.0.1:{cp}','server':f'127.0.0.1:{sp}', 'tls':tls|{'ca_file':str(cert)}}
    processes, logs, perfs = [], [], []

    def launch(cmd, cpu, **kwargs):
        log = (folder/f'process-{len(processes)}.log').open('w')
        logs.append(log)
        p = subprocess.Popen(['taskset','-c',str(cpu),*map(str, cmd)], env=os.environ|{'GOMAXPROCS':'1'},
                             stdout=kwargs.pop('stdout',log), stderr=log, **kwargs)
        processes.append(p)
        return p

    try:
        for name, cfg in [('server',sc),('client',cc)]:
            path=folder/f'{name}.json'
            path.write_text(json.dumps(cfg))
            path.chmod(0o600)
        backend=launch([args.peer,'-role','backend','-listen',f'127.0.0.1:{bp}'],args.cpus[2]);wait(bp,backend)
        server=launch([binaries[server_name],'-config',folder/'server.json'],args.cpus[0]);wait(sp,server)
        client=launch([binaries[client_name],'-config',folder/'client.json'],args.cpus[1]);wait(cp,client)
        load=launch([args.peer,'-socks',f'127.0.0.1:{cp}','-target',f'127.0.0.1:{bp}',
                     '-mode',mode,'-connections',concurrency,'-bytes',args.size,'-rounds',args.rounds],args.cpus[3],stdout=subprocess.PIPE,stdin=subprocess.PIPE)
        assert line(load)['ready']
        physical_sessions=sum(1 for row in Path('/proc/net/tcp').read_text().splitlines()[1:] if int(row.split()[2].split(':')[1],16)==sp and row.split()[3]=='01')
        before=[counters(p.pid) for p in [server,client]]
        if not args.no_perf:
            for name,p in [('server',server),('client',client)]:
                perf=Perf(p.pid,folder/name);perfs.append(perf);perf.command('enable')
        load.stdin.write(b'\n');load.stdin.flush()
        result=line(load)
        for perf in perfs:
            perf.command('disable')
        after=[counters(p.pid) for p in [server,client]]
        load.wait(timeout=5)
        if load.returncode: raise RuntimeError('benchmark exit failure')
        for i,name in enumerate(['server','client']):
            result[name] = {'cpu_seconds': after[i]['cpu_seconds']-before[i]['cpu_seconds'], 'peak_rss_kib':after[i]['peak_rss_kib']}
        result |= {'pair':pair,'concurrency':concurrency,'physical_sessions':physical_sessions}
        for name,perf in zip(['server','client'],perfs):
            perf.close();result[name]['perf']=perf.result()
            result[name]['cpu_seconds']=result[name]['perf']['task-clock']/1000
        perfs.clear()
        result['gbit_s']=result['bytes']*8/result['seconds']/1e9
        cpu_seconds=sum(result[x]['cpu_seconds'] for x in ['server','client'])
        result['gib_per_cpu_second']=result['bytes']/2**30/cpu_seconds if cpu_seconds>0 else None
        (folder/'result.json').write_text(json.dumps(result,indent=2)+'\n')
        return result
    finally:
        for perf in perfs: perf.close()
        for proc in reversed(processes):
            if proc.poll() is None:
                proc.terminate()
                try: proc.wait(timeout=5)
                except subprocess.TimeoutExpired: proc.kill();proc.wait()
        for log in logs: log.close()
        for name in ['client.json','server.json']:
            (folder/name).unlink(missing_ok=True)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--go',type=Path,default=ROOT/'.build/rust-evaluation/veil-go-batch')
    p.add_argument('--openssl',type=Path,default=ROOT/'.build/rust-evaluation/veil-go-openssl')
    p.add_argument('--rust',type=Path,default=ROOT/'veil-rust/target/release/veil-rust')
    p.add_argument('--peer',type=Path,default=ROOT/'.build/rust-evaluation/benchpeer')
    p.add_argument('--output',type=Path,required=True)
    p.add_argument('--pairs',default='go-go,rust-rust,go-rust,rust-go,go-openssl')
    p.add_argument('--modes',default='U1,D1,U8,D8,E1,S1')
    p.add_argument('--size',type=int,default=2<<30)
    p.add_argument('--rounds',type=int,default=5000)
    p.add_argument('--repeats',type=int,default=5)
    p.add_argument('--cpus',type=lambda s:list(map(int,s.split(','))),default=[14,15,16,17])
    p.add_argument('--no-perf',action='store_true')
    args=p.parse_args();args.output.mkdir(parents=True,exist_ok=True)
    metadata={'cpus':args.cpus,'size':args.size,'rounds':args.rounds,'repeats':args.repeats,
              'transport':'TLS 1.3 AES-128-GCM, two-lane pool policy, no TLS padding; handshake excluded (Go default hybrid group, Rust X25519)',
              'binaries': {n:hashlib.sha256(getattr(args,n).read_bytes()).hexdigest() for n in ['go','rust','openssl','peer']},
              'host':subprocess.check_output(['lscpu'],text=True),'date':time.strftime('%Y-%m-%d %H:%M:%S %z')}
    (args.output/'metadata.json').write_text(json.dumps(metadata,indent=2)+'\n')
    cases=list(itertools.product(args.pairs.split(','),args.modes.split(','),range(args.repeats)))
    random.Random(20260929).shuffle(cases)
    results=[]
    with tempfile.TemporaryDirectory(prefix='veil-rust-bench-') as temp:
        cert,key=[Path(temp)/s for s in ['cert.pem','key.pem']]
        subprocess.run(['openssl','req','-x509','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-days','1',
                        '-subj','/CN=cover.test','-addext','subjectAltName=DNS:cover.test','-addext','basicConstraints=critical,CA:FALSE',
                        '-keyout',str(key),'-out',str(cert)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        for i,(pair,mode,repeat) in enumerate(cases):
            result=trial(args,pair,mode[0],int(mode[1:]),args.output/f'{i:03d}-{pair}-{mode}-{repeat}',cert,key)
            results.append(result)
            print(json.dumps({'trial':i+1,'of':len(cases),**result}),flush=True)
    summary=[]
    for pair,mode in itertools.product(args.pairs.split(','),args.modes.split(',')):
        rows=[r for r in results if r['pair']==pair and r['mode']==mode[0] and r['concurrency']==int(mode[1:])]
        entry={'pair':pair,'mode':mode,'samples':len(rows)}
        for field in ['gbit_s','gib_per_cpu_second','seconds','p50_us','p99_us']:
            values=[r[field] for r in rows if r.get(field) is not None]
            if values: entry[field]={'median':statistics.median(values),'min':min(values),'max':max(values)}
        for side in ['server','client']:
            entry[side+'_cpu_seconds']=statistics.median(r[side]['cpu_seconds'] for r in rows)
            entry[side+'_peak_rss_kib']=statistics.median(r[side]['peak_rss_kib'] for r in rows)
        summary.append(entry)
    (args.output/'summary.json').write_text(json.dumps(summary,indent=2)+'\n')


if __name__=='__main__': main()
