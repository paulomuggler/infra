import { Sandbox } from "e2b";

async function main() {
  const sandbox = await Sandbox.create("kiln-dev", { timeoutMs: 300000 });
  console.log(`Sandbox: ${sandbox.sandboxId}`);

  // Start a simple HTTP server inside the sandbox
  await sandbox.commands.run("node -e \"require('http').createServer((req,res)=>{res.end('Hello from Kiln sandbox!')}).listen(8080)\" &", { background: true });

  // Give it a moment to start
  await new Promise(r => setTimeout(r, 1000));

  // Test from inside the sandbox
  const curlResult = await sandbox.commands.run("curl -sf http://localhost:8080");
  console.log("Internal access:", curlResult.stdout.trim());

  // Get the external host URL
  const host = sandbox.getHost(8080);
  console.log("External URL:", host);

  // Test outbound internet access
  const outbound = await sandbox.commands.run("curl -sf -o /dev/null -w %{http_code} https://httpbin.org/get", { timeoutMs: 15000 });
  console.log("Outbound internet:", outbound.stdout.trim());

  await sandbox.kill();
  console.log("Done");
}

main().catch(console.error);
