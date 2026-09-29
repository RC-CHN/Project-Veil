#!/usr/bin/env python3
"""Exercise the PHP platform adapter with a real private Go daemon in Docker."""
import argparse
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--daemon', type=Path, required=True)
    args = parser.parse_args()
    image = 'veil-php-test:' + uuid.uuid4().hex
    subprocess.run(['docker', 'build', '-q', '--network', 'host', '--build-arg', 'http_proxy', '--build-arg', 'https_proxy', '-t', image, '-'], input=b'FROM alpine:3.22\nRUN apk add --no-cache php84\n', check=True, timeout=180)
    try:
        run(args.daemon, image)
    finally:
        subprocess.run(['docker', 'image', 'rm', image], check=True, stdout=subprocess.DEVNULL)


def run(binary, image):
    with tempfile.TemporaryDirectory(prefix='veil-opn-catalog-') as directory:
        base = Path(directory)
        runtime = base / 'runtime'
        runtime.mkdir(mode=0o700)
        socket = runtime / 'control.sock'
        with (base / 'daemon.log').open('w+') as log:
            daemon = subprocess.Popen([str(binary.resolve()), '-state-dir', str(base / 'state'),
                                       '-socket', str(socket)], stdout=log, stderr=log)
            try:
                for _ in range(100):
                    if socket.exists():
                        break
                    if daemon.poll() is not None:
                        raise RuntimeError('test daemon exited')
                    time.sleep(.05)
                subprocess.run(['docker', 'run', '--rm', '--network', 'host',
                                '-v', f'{runtime}:/var/run/veil', '-v', f'{ROOT}:/src:ro',
                                image, 'php84', '/src/scripts/opnsense_catalog_test.php',
                                '/src/platform/opnsense/src/opnsense/mvc/app/library/OPNsense/Veil'], check=True, timeout=90)
            finally:
                daemon.terminate()
                daemon.wait(timeout=10)


if __name__ == '__main__':
    main()
