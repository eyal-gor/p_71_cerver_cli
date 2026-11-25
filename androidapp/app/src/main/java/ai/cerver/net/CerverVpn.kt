package ai.cerver.net

import ai.cerver.cervernet.Cervernet
import android.content.Intent
import android.net.VpnService
import kotlin.concurrent.thread

// The phone's side of your Cerver network. Android gives us a tunnel for
// 100.96.0.0/16 (your devices) and 100.100.100.100 (the name server inside the
// app); Cerver's engine (Go, same as the Mac's) runs it. The app itself is
// left out of the tunnel, so the engine's own traffic goes over Wi-Fi or
// mobile data.
class CerverVpn : VpnService() {
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == STOP) {
            stopEngine()
            return START_NOT_STICKY
        }
        val ip = intent?.getStringExtra("ip") ?: return START_NOT_STICKY
        val key = intent.getStringExtra("key") ?: return START_NOT_STICKY
        val token = intent.getStringExtra("token") ?: return START_NOT_STICKY
        val name = intent.getStringExtra("name") ?: "android"
        thread {
            try {
                val pfd = Builder()
                    .setSession("Cerver")
                    .setMtu(1280)
                    .addAddress(ip, 32)
                    .addRoute("100.96.0.0", 16)
                    .addRoute("100.100.100.100", 32)
                    .addDnsServer("100.100.100.100")
                    .addSearchDomain("cerver")
                    .addDisallowedApplication(packageName)
                    .establish() ?: throw IllegalStateException("Android didn't allow the connection")
                Cervernet.start(pfd.detachFd(), key, Net.GATEWAY, token, name)
                Net.engineStarted()
            } catch (e: Exception) {
                Net.engineFailed(e.message ?: "Couldn't connect")
                stopSelf()
            }
        }
        return START_STICKY
    }

    private fun stopEngine() {
        Cervernet.stop()
        Net.engineStopped()
        stopSelf()
    }

    override fun onRevoke() { // turned off from Android's VPN settings
        stopEngine()
        super.onRevoke()
    }

    override fun onDestroy() {
        Cervernet.stop()
        super.onDestroy()
    }

    companion object { const val STOP = "ai.cerver.net.STOP" }
}
