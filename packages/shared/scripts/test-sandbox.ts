import { Sandbox } from "e2b";

async function main() {
  console.log("Creating sandbox...");
  const start = Date.now();
  const sandbox = await Sandbox.create("kiln-dev", { timeoutMs: 300000 });
  console.log(`Created in ${Date.now() - start}ms: ${sandbox.sandboxId}`);

  const nodeResult = await sandbox.commands.run("node --version");
  console.log("Node:", nodeResult.stdout.trim());

  const pythonResult = await sandbox.commands.run("python3 --version");
  console.log("Python:", pythonResult.stdout.trim());

  const start2 = Date.now();
  const sandbox2 = await Sandbox.create("kiln-dev", { timeoutMs: 300000 });
  console.log(`Second sandbox in ${Date.now() - start2}ms: ${sandbox2.sandboxId}`);

  await sandbox.kill();
  await sandbox2.kill();
  console.log("Done");
}

main().catch(console.error);
