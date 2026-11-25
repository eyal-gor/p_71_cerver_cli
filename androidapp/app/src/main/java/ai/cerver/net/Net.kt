package ai.cerver.net

import ai.cerver.cervernet.Cervernet
import android.app.Application
import android.content.Context
import android.content.Intent
import android.os.Build
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
import java.net.URL

// The phone joins your Cerver network with Cerver's own engine (CerverVpn +
// the Go library): direct connections, the backup route when direct fails,
// and names (mac-mini, mac-mini.cerver) answered inside the app.

class App : Application() {
    override fun onCreate() {
        super.onCreate()
        Net.init(this)
    }
}

data class Computer(
    val name: String, val host: String, val ip: String,
    val online: Boolean, val connected: Boolean, val via: String,
)

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
    fun call(method: String, path: String, key: String?, body: JSONObject? = null): JSONObject {
        val conn = URL(Net.GATEWAY + path).openConnection() as HttpURLConnection
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
    const val GATEWAY = "https://gateway.cerver.ai"
    private lateinit var app: Context
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val _state = MutableStateFlow(UiState())
    val state: StateFlow<UiState> = _state
    private var loop: Job? = null

    private val prefs by lazy { app.getSharedPreferences("cerver", Context.MODE_PRIVATE) }

    fun init(context: Context) {
        app = context.applicationContext
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

    private fun privateKey(): String =
        prefs.getString("private_key", null) ?: Cervernet.newPrivateKey().also { prefs.edit().putString("private_key", it).apply() }

    private fun isComputer(os: String) = os != "phone" && os != "android" && os != "ios"

    /** While off: check in (no addresses to share) to get this phone's address and list your computers. */
    private fun checkIn(): String {
        val key = prefs.getString("api_key", null) ?: throw ApiError("Sign in first.")
        val ans = Api.call(
            "POST", "/v2/net/nodes", key,
            JSONObject().put("public_key", Cervernet.publicKey(privateKey())).put("name", _state.value.name)
                .put("endpoints", JSONArray()).put("os", "android"),
        )
        prefs.edit().putString("node_id", ans.optString("node_id")).apply()
        val list = mutableListOf<Computer>()
        val peers = ans.optJSONArray("peers") ?: JSONArray()
        for (i in 0 until peers.length()) {
            val p = peers.getJSONObject(i)
            if (!isComputer(p.optString("os"))) continue
            val name = p.optString("name")
            val label = p.optString("label")
            list += Computer(label.ifEmpty { name }, hostName(label.ifEmpty { name }), p.getString("ip"), p.optBoolean("online"), false, "")
        }
        val ip = ans.getString("ip")
        _state.update { it.copy(ip = ip, computers = list, error = null) }
        return ip
    }

    private fun hostName(n: String) = n.lowercase().replace("'", "").replace(Regex("[^a-z0-9]+"), "-").trim('-')

    /** While on: the engine's own view (who's connected, direct or backup route). */
    private fun readEngine() {
        val s = JSONObject(Cervernet.status())
        if (!s.optBoolean("running")) return
        val list = mutableListOf<Computer>()
        val peers = s.optJSONArray("peers") ?: JSONArray()
        for (i in 0 until peers.length()) {
            val p = peers.getJSONObject(i)
            if (!isComputer(p.optString("os"))) continue
            list += Computer(
                p.optString("label").ifEmpty { p.optString("name") }, p.optString("host"), p.optString("ip"),
                p.optBoolean("online"), p.optBoolean("connected"), p.optString("via"),
            )
        }
        _state.update { it.copy(ip = s.optString("ip"), computers = list) }
    }

    private fun startLoop() {
        loop?.cancel()
        loop = scope.launch {
            while (true) {
                try {
                    if (_state.value.connected) readEngine() else checkIn()
                } catch (e: Exception) {
                    _state.update { it.copy(error = e.message) }
                }
                delay(if (_state.value.connected) 3_000 else 30_000)
            }
        }
    }

    /** Call after Android's VPN permission was granted. */
    fun connect() {
        scope.launch {
            _state.update { it.copy(busy = true, error = null) }
            try {
                val ip = checkIn()
                val i = Intent(app, CerverVpn::class.java)
                    .putExtra("ip", ip).putExtra("key", privateKey())
                    .putExtra("token", prefs.getString("api_key", null)).putExtra("name", _state.value.name)
                app.startService(i)
            } catch (e: Exception) {
                _state.update { it.copy(busy = false, error = e.message ?: "Couldn't connect") }
            }
        }
    }

    fun disconnect() {
        app.startService(Intent(app, CerverVpn::class.java).setAction(CerverVpn.STOP))
    }

    internal fun engineStarted() = _state.update { it.copy(connected = true, busy = false, error = null) }
    internal fun engineFailed(msg: String) = _state.update { it.copy(connected = false, busy = false, error = msg) }
    internal fun engineStopped() = _state.update { it.copy(connected = false, busy = false) }

    fun refresh() {
        scope.launch {
            try { if (_state.value.connected) readEngine() else checkIn() } catch (e: Exception) { _state.update { it.copy(error = e.message) } }
        }
    }

    /** Removes this phone from the network and signs out. */
    fun signOut() {
        disconnect()
        scope.launch {
            loop?.cancel()
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
