package ai.cerver.net

import android.content.Context
import android.content.Intent
import androidx.core.content.FileProvider
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.File
import java.net.HttpURLConnection
import java.net.URL

// Updates: cerver.ai/downloads/android.json says which version is current.
// "Update" downloads it and hands it to Android, which asks you to confirm
// the install (an app can't skip that). Same signing key, so it updates in
// place and keeps your sign-in.

data class UpdateState(
    val latest: String? = null,     // newer version available, e.g. "1.2"
    val progress: Int? = null,      // 0-100 while downloading
    val checking: Boolean = false,
    val message: String? = null,
)

object Updater {
    private const val INFO = "https://cerver.ai/downloads/android.json"
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val _state = MutableStateFlow(UpdateState())
    val state: StateFlow<UpdateState> = _state
    private var url: String? = null

    val current: String get() = BuildConfig.VERSION_NAME

    fun check(quiet: Boolean = true) {
        scope.launch {
            _state.value = _state.value.copy(checking = true, message = null)
            try {
                val c = URL(INFO + "?v=" + System.currentTimeMillis()).openConnection() as HttpURLConnection
                c.connectTimeout = 10_000; c.readTimeout = 10_000
                val j = JSONObject(c.inputStream.bufferedReader().use { it.readText() })
                if (j.optInt("version_code") > BuildConfig.VERSION_CODE) {
                    url = j.optString("url").let { if (it.startsWith("http")) it else "https://cerver.ai$it" }
                    _state.value = UpdateState(latest = j.optString("version"))
                } else {
                    _state.value = UpdateState(message = if (quiet) null else "You have the latest version ($current).")
                }
            } catch (e: Exception) {
                _state.value = UpdateState(message = if (quiet) null else "Couldn't check for updates. Try again.")
            }
        }
    }

    fun update(ctx: Context) {
        val from = url ?: return
        scope.launch {
            try {
                val dir = File(ctx.cacheDir, "updates").apply { mkdirs() }
                val apk = File(dir, "cerver.apk")
                val c = URL(from).openConnection() as HttpURLConnection
                c.connectTimeout = 15_000; c.readTimeout = 30_000
                val total = c.contentLengthLong
                c.inputStream.use { input ->
                    apk.outputStream().use { out ->
                        val buf = ByteArray(64 * 1024)
                        var done = 0L
                        while (true) {
                            val n = input.read(buf)
                            if (n < 0) break
                            out.write(buf, 0, n)
                            done += n
                            if (total > 0) _state.value = _state.value.copy(progress = (done * 100 / total).toInt())
                        }
                    }
                }
                val uri = FileProvider.getUriForFile(ctx, "ai.cerver.net.files", apk)
                withContext(Dispatchers.Main) {
                    ctx.startActivity(
                        Intent(Intent.ACTION_VIEW).setDataAndType(uri, "application/vnd.android.package-archive")
                            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK),
                    )
                }
                _state.value = _state.value.copy(progress = null)
            } catch (e: Exception) {
                _state.value = _state.value.copy(progress = null, message = "The download failed. Try again.")
            }
        }
    }
}
