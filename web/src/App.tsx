// Holding page. The API is live and complete; the interface is not built yet,
// so this says so rather than showing an empty file browser. It is replaced
// wholesale by the real routes and screens.
function App() {
  return (
    <main className="mx-auto flex min-h-screen max-w-xl flex-col justify-center gap-4 px-6">
      <h1 className="text-3xl font-semibold tracking-tight">Drive</h1>
      <p className="text-base leading-relaxed text-neutral-600">
        A self-hosted file platform. Uploads are resumable by design: a transfer
        survives a lost connection, a closed laptop, a restarted server, and
        picks up from the last confirmed part.
      </p>
      <p className="text-base leading-relaxed text-neutral-600">
        The server is running here. The web interface is still being built, so
        there is nothing to sign in to yet.
      </p>
    </main>
  )
}

export default App
