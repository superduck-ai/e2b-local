const sdkImport = process.env.E2B_JS_SDK_IMPORT || 'e2b'
const apiURL = process.env.E2B_API_URL
let sandboxURL = process.env.E2B_SANDBOX_URL

if (!apiURL) {
  throw new Error('E2B_API_URL is required, for example http://127.0.0.1:3000')
}

const { Sandbox } = await import(sdkImport)

console.log(`api url: ${apiURL}`)
console.log(`sandbox url: ${sandboxURL || '(from create response)'}`)

const sandbox = await Sandbox.create()
console.log(`sandbox created: ${sandbox.sandboxId}`)

try {
  const result = await sandbox.commands.run('echo "hello"')

  if (result.exitCode !== 0) {
    throw new Error(`expected exitCode 0, got ${result.exitCode}`)
  }

  if (!result.stdout.includes('hello')) {
    throw new Error(`expected stdout to include hello, got ${JSON.stringify(result.stdout)}`)
  }

  console.log(JSON.stringify(result))
} finally {
  await sandbox.kill()
  console.log('sandbox killed')
}
