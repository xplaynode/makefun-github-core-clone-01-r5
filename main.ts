// Entry launcher: the harness runs this file with Node; the server itself is
// the compiled Go binary shipped beside it (`forge`). The launcher starts
// the binary with the process environment unchanged, mirrors its stdio, and
// exits with the binary's exit code once it finishes.

import { spawn } from 'node:child_process';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const binary = `${here}/forge`;

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start the server binary: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.stderr.write(`[entry] server binary stopped by ${signal}\n`);
    process.exit(1);
    return;
  }
  process.exit(code ?? 1);
});
