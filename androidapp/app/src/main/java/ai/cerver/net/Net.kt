package ai.cerver.net

import android.app.Application
import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.Build
import com.wireguard.android.backend.GoBackend
import com.wireguard.android.backend.Tunnel
import com.wireguard.config.Config
import com.wireguard.config.InetEndpoint
import com.wireguard.config.InetNetwork
import com.wireguard.config.Interface
import com.wireguard.config.Peer
import com.wireguard.crypto.Key
import com.wireguard.crypto.KeyPair
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.InetAddress
import java.net.URL

// The phone joins your Cerver network the same way `cerver net phone` does:
// it makes its own WireGuard key, checks in with the gateway, and gets an
// address plus your computers' keys and addresses. Unlike a QR code, the app
// keeps checking in, so it follows computers whose addresses change and picks
// the home address on Wi-Fi and the public one on mobile data.

class App : Application() {
    override fun onCreate() {
        super.onCreate()
        Net.init(this)
    }
}

data class Computer(val name: String, val ip: String, val online: Boolean, val reachable: Boolean)

data class UiState(
    val signedIn: Boolean = false,
    val userCode: String? = null,
    val verifyUrl: String? = null,
    val connected: Boolean = false,
    val busy: Boolean = false,
    val ip: String? = null,
    val name: String = "",
    val computers: List<Computer> = emptyList(),
    val error: String? = null,
)

private class ApiError(msg: String) : Exception(msg)

private object Api {
    const val BASE = "https://gateway.cerver.ai"

    fun call(method: String, path: String, key: String?, body: JSONObject? = null): JSONObject {
        val conn = URL(BASE + path).openConnection() as HttpURLConnection
        conn.requestMethod = method
        conn.connectTimeout = 15_000
        conn.readTimeout = 20_000
        conn.setRequestProperty("Accept", "application/json")
        if (key != null) conn.setRequestProperty("Authorization", "Bearer $key")
        if (body != null) {
            conn.doOutput = true
            conn.setRequestProperty("Content-Type", "application/json")
            conn.outputStream.use { it.write(body.toString().toByteArray()) }
        }
        val code = conn.responseCode
        val text = (if (code < 400) conn.inputStream else conn.errorStream)?.bufferedReader()?.use { it.readText() } ?: ""
        conn.disconnect()
        val json = runCatching { JSONObject(text) }.getOrElse { JSONObject() }
        if (code >= 400) {
            if (code == 401) throw ApiError("Your sign-in expired. Sign in again.")
            throw ApiError(json.optString("error").ifEmpty { "Cerver answered $code" })
        }
        return json
    }
}

object Net {
    private lateinit var app: Context
    private lateinit var backend: GoBackend
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val _state = MutableStateFlow(UiState())
    val state: StateFlow<UiState> = _state
    private var loop: Job? = null
    private var lastConfig: String? = null

    private val prefs by lazy { app.getSharedPreferences("cerver", Context.MODE_PRIVATE) }

    private object CerverTunnel : Tunnel {
        override fun getName() = "cerver"
        override fun onStateChange(newState: Tunnel.State) {
            _state.update { it.copy(connected = newState == Tunnel.State.UP) }
        }
    }

    fun init(context: Context) {
        app = context.applicationContext
        backend = GoBackend(app)
        val name = prefs.getString("name", null) ?: deviceName().also { prefs.edit().putString("name", it).apply() }
        _state.value = UiState(signedIn = prefs.getString("api_key", null) != null, name = name)
        if (_state.value.signedIn) startLoop()
    }

    private fun deviceName(): String {
        val clean = "${Build.MANUFACTURER} ${Build.MODEL}".lowercase().replace(Regex("[^a-z0-9]+"), "-").trim('-')
        return clean.ifEmpty { "android" }.take(40)
    }

    // ---- sign in (device code, same flow as `cerver login`) ----

    fun signIn(open: (String) -> Unit) {
        scope.launch {
            _state.update { it.copy(busy = true, error = null) }
            try {
                val start = Api.call("POST", "/v2/auth/device", null, JSONObject().put("machine_name", _state.value.name))
                val url = start.getString("verification_uri")
                _state.update { it.copy(userCode = start.optString("user_code"), verifyUrl = url, busy = false) }
                launch(Dispatchers.Main) { open(url) }
                val interval = start.optInt("interval", 5).coerceIn(3, 10) * 1000L
                val deadline = System.currentTimeMillis() + start.optInt("expires_in", 600) * 1000L
                while (System.currentTimeMillis() < deadline && _state.value.userCode != null) {
                    delay(interval)
                    val poll = runCatching {
                        Api.call("GET", "/v2/auth/device?device_code=" + start.getString("device_code"), null)
                    }.getOrNull() ?: continue
                    val token = poll.optString("access_token")
                    if (poll.optString("status") == "approved" && token.isNotEmpty()) {
                        prefs.edit().putString("api_key", token).apply()
                        _state.update { it.copy(signedIn = true, userCode = null, verifyUrl = null) }
                        startLoop()
                        return@launch
                    }
                    when (poll.optString("error")) {
                        "expired_token" -> throw ApiError("The code expired. Try again.")
                        "access_denied" -> throw ApiError("Sign-in was declined in the browser.")
                        "invalid_device_code" -> throw ApiError("The code wasn't accepted. Try again.")
                    }
                }
                if (_state.value.userCode != null) throw ApiError("Timed out waiting for approval. Try again.")
            } catch (e: Exception) {
                _state.update { it.copy(busy = false, userCode = null, verifyUrl = null, error = e.message) }
            }
        }
    }

    fun cancelSignIn() = _state.update { it.copy(userCode = null, verifyUrl = null) }

    // ---- the network ----

    private fun keyPair(): KeyPair {
        val saved = prefs.getString("private_key", null)
        if (saved != null) return KeyPair(Key.fromBase64(saved))
        val kp = KeyPair()
        prefs.edit().putString("private_key", kp.privateKey.toBase64()).apply()
        return kp
    }

    private fun onWifi(): Boolean {
        val cm = app.getSystemService(ConnectivityManager::class.java)
        val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return false
        return caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) || caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)
    }

    private fun isPrivate(host: String): Boolean = runCatching {
        val a = InetAddress.getByName(host)
        a.isSiteLocalAddress && !host.startsWith("100.")
    }.getOrDefault(false)

    // On Wi-Fi try the computer's home-network address first (works when the
    // phone is home); otherwise its public one.
    private fun pickEndpoint(eps: JSONArray): String? {
        var lan: String? = null
        var public: String? = null
        for (i in 0 until eps.length()) {
            val e = eps.optString(i)
            val host = e.substringBeforeLast(':')
            if (host.contains(':') || host.isEmpty()) continue // IPv6: skip for now
            if (isPrivate(host)) { if (lan == null) lan = e } else if (public == null) public = e
        }
        return if (onWifi()) lan ?: public else public ?: lan
    }

    /** Check in with the gateway; returns the tunnel config for the current network. */
    private fun checkIn(): Config {
        val key = prefs.getString("api_key", null) ?: throw ApiError("Sign in first.")
        val kp = keyPair()
        val ans = Api.call(
            "POST", "/v2/net/nodes", key,
            JSONObject().put("public_key", kp.publicKey.toBase64()).put("name", _state.value.name)
                .put("endpoints", JSONArray()).put("os", "phone"),
        )
        val ip = ans.getString("ip")
        prefs.edit().putString("node_id", ans.optString("node_id")).apply()
        val cfg = Config.Builder().setInterface(
            Interface.Builder().addAddress(InetNetwork.parse("$ip/32")).setKeyPair(kp).build(),
        )
        val computers = mutableListOf<Computer>()
        val peers = ans.optJSONArray("peers") ?: JSONArray()
        for (i in 0 until peers.length()) {
            val p = peers.getJSONObject(i)
            if (p.optString("os") == "phone") continue // phones can't reach each other
            val ep = pickEndpoint(p.optJSONArray("endpoints") ?: JSONArray())
            val pip = p.getString("ip")
            computers += Computer(p.optString("name"), pip, p.optBoolean("online"), ep != null)
            if (ep == null) continue
            cfg.addPeer(
                Peer.Builder().parsePublicKey(p.getString("public_key"))
                    .addAllowedIp(InetNetwork.parse("$pip/32"))
                    .setEndpoint(InetEndpoint.parse(ep))
                    .setPersistentKeepalive(25).build(),
            )
        }
        _state.update { it.copy(ip = ip, computers = computers, error = null) }
        return cfg.build()
    }

    private fun startLoop() {
        loop?.cancel()
        loop = scope.launch {
            while (true) {
                try {
                    val cfg = checkIn()
                    val text = cfg.toWgQuickString()
                    if (_state.value.connected && text != lastConfig) {
                        backend.setState(CerverTunnel, Tunnel.State.UP, cfg)
                    }
                    lastConfig = text
                } catch (e: Exception) {
                    _state.update { it.copy(error = e.message) }
                }
                delay(if (_state.value.connected) 30_000 else 60_000)
            }
        }
    }

    /** Call after the system VPN permission was granted. */
    fun connect() {
        scope.launch {
            _state.update { it.copy(busy = true, error = null) }
            try {
                val cfg = checkIn()
                lastConfig = cfg.toWgQuickString()
                backend.setState(CerverTunnel, Tunnel.State.UP, cfg)
                _state.update { it.copy(connected = true) }
            } catch (e: Exception) {
                _state.update { it.copy(error = e.message ?: "Couldn't connect") }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    fun disconnect() {
        scope.launch {
            runCatching { backend.setState(CerverTunnel, Tunnel.State.DOWN, null) }
            _state.update { it.copy(connected = false) }
        }
    }

    fun refresh() {
        scope.launch {
            try {
                val cfg = checkIn()
                if (_state.value.connected) backend.setState(CerverTunnel, Tunnel.State.UP, cfg)
                lastConfig = cfg.toWgQuickString()
            } catch (e: Exception) {
                _state.update { it.copy(error = e.message) }
            }
        }
    }

    /** Removes this phone from the network and signs out. */
    fun signOut() {
        scope.launch {
            loop?.cancel()
            runCatching { backend.setState(CerverTunnel, Tunnel.State.DOWN, null) }
            val key = prefs.getString("api_key", null)
            val node = prefs.getString("node_id", null)
            if (key != null) {
                if (!node.isNullOrEmpty()) runCatching { Api.call("DELETE", "/v2/net/nodes/$node", key) }
                runCatching { Api.call("DELETE", "/v2/auth/keys/$key", key) }
            }
            prefs.edit().remove("api_key").remove("private_key").remove("node_id").apply()
            _state.value = UiState(name = _state.value.name)
        }
    }
}
