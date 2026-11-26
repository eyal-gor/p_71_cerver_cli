package ai.cerver.net

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.provider.OpenableColumns
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

// Phone asks: an agent on one of your computers asks this phone for photos or
// files (`cerver phone pull`). You see who asks and why, and Approve opens
// Android's own picker: only what you pick is sent, straight to that computer
// over your Cerver network. Nothing goes through Cerver's servers but the ask.

data class Ask(val id: String, val kind: String, val reason: String, val max: Int, val from: String, val uploadUrl: String)

data class AsksState(val asks: List<Ask> = emptyList(), val sending: String? = null, val note: String? = null)

object Asks {
    private val _state = MutableStateFlow(AsksState())
    val state: StateFlow<AsksState> = _state
    private val notified = mutableSetOf<String>()
    private const val CHANNEL = "asks"

    /** Called from the app's loop with the gateway's pending asks for this phone. */
    internal fun update(ctx: Context, json: JSONObject) {
        val arr = json.optJSONArray("asks") ?: return
        val list = (0 until arr.length()).map { i ->
            val a = arr.getJSONObject(i)
            Ask(a.getString("id"), a.optString("kind"), a.optString("reason"), a.optInt("max", 10), a.optString("from"), a.optString("upload_url"))
        }
        _state.update { it.copy(asks = list) }
        list.filter { notified.add(it.id) }.forEach { notify(ctx, it) }
    }

    private fun notify(ctx: Context, a: Ask) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL, "Requests from your computers", NotificationManager.IMPORTANCE_HIGH))
        val open = PendingIntent.getActivity(
            ctx, a.id.hashCode(), Intent(ctx, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val n = NotificationCompat.Builder(ctx, CHANNEL)
            .setSmallIcon(R.drawable.ic_cerver_grid)
            .setContentTitle("${a.from} asks for ${a.kind}")
            .setContentText(a.reason)
            .setStyle(NotificationCompat.BigTextStyle().bigText("${a.reason}\nOpen Cerver to choose what to send, or deny."))
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .build()
        runCatching { NotificationManagerCompat.from(ctx).notify(a.id.hashCode(), n) }
    }

    private fun drop(id: String, note: String) = _state.update { s -> s.copy(asks = s.asks.filter { it.id != id }, sending = null, note = note) }

    fun clearNote() = _state.update { it.copy(note = null) }

    /** Deny: the computer is told right away. */
    internal fun deny(key: String, a: Ask) {
        runCatching { Net.api("POST", "/v2/net/asks/${a.id}/decide", key, JSONObject().put("decision", "deny")) }
        drop(a.id, "Denied: nothing was sent to ${a.from}.")
    }

    /** Send what you picked straight to the asking computer, then tell the gateway it's approved. */
    internal fun send(ctx: Context, key: String, a: Ask, uris: List<Uri>) {
        if (uris.isEmpty()) return
        _state.update { it.copy(sending = a.id, note = null) }
        try {
            val boundary = "cerver" + System.nanoTime()
            val conn = URL(a.uploadUrl).openConnection() as HttpURLConnection
            conn.requestMethod = "POST"
            conn.doOutput = true
            conn.connectTimeout = 15_000
            conn.readTimeout = 120_000
            conn.setChunkedStreamingMode(64 * 1024)
            conn.setRequestProperty("Content-Type", "multipart/form-data; boundary=$boundary")
            conn.outputStream.use { out ->
                uris.take(a.max).forEachIndexed { i, uri ->
                    val name = displayName(ctx, uri) ?: "file-${i + 1}"
                    val type = ctx.contentResolver.getType(uri) ?: "application/octet-stream"
                    out.write("--$boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"${name.replace("\"", "")}\"\r\nContent-Type: $type\r\n\r\n".toByteArray())
                    ctx.contentResolver.openInputStream(uri)?.use { it.copyTo(out) }
                    out.write("\r\n".toByteArray())
                }
                out.write("--$boundary--\r\n".toByteArray())
            }
            val code = conn.responseCode
            val body = (if (code < 400) conn.inputStream else conn.errorStream)?.bufferedReader()?.use { it.readText() } ?: ""
            conn.disconnect()
            if (code >= 400) throw IllegalStateException("${a.from} answered $code")
            val n = runCatching { JSONObject(body).optInt("received") }.getOrDefault(uris.size)
            runCatching { Net.api("POST", "/v2/net/asks/${a.id}/decide", key, JSONObject().put("decision", "approve").put("count", n)) }
            drop(a.id, "Sent $n to ${a.from}.")
        } catch (e: Exception) {
            _state.update { it.copy(sending = null, note = "Couldn't reach ${a.from}: ${e.message ?: "no connection"}. Is Cerver on, and is ${a.from} online?") }
        }
    }

    private fun displayName(ctx: Context, uri: Uri): String? = runCatching {
        ctx.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst()) c.getString(0) else null
        }
    }.getOrNull()
}
