package ai.cerver.net

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.net.Uri
import android.net.VpnService
import android.os.Bundle
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

private val Green = Color(0xFF0F7A5A)
private val GreenDark = Color(0xFF3FC595)

class MainActivity : ComponentActivity() {
    private val askVpn = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { r ->
        if (r.resultCode == RESULT_OK) Net.connect()
        else Toast.makeText(this, "Android needs your OK to connect", Toast.LENGTH_SHORT).show()
    }

    // Phone asks: the picker for the ask being answered.
    private var answering: Ask? = null
    private val pickMedia = registerForActivityResult(ActivityResultContracts.PickMultipleVisualMedia(50)) { uris ->
        answering?.let { a -> if (uris.isNotEmpty()) Net.sendAsk(a, uris.take(a.max)) }
    }
    private val pickOneMedia = registerForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri ->
        answering?.let { a -> if (uri != null) Net.sendAsk(a, listOf(uri)) }
    }
    private val pickFiles = registerForActivityResult(ActivityResultContracts.OpenMultipleDocuments()) { uris ->
        answering?.let { a -> if (uris.isNotEmpty()) Net.sendAsk(a, uris.take(a.max)) }
    }
    private val askNotify = registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    private fun choose(a: Ask) {
        answering = a
        val media = ActivityResultContracts.PickVisualMedia.ImageAndVideo
        when {
            a.kind == "files" -> pickFiles.launch(arrayOf("*/*"))
            a.max <= 1 -> pickOneMedia.launch(androidx.activity.result.PickVisualMediaRequest(media))
            else -> pickMedia.launch(androidx.activity.result.PickVisualMediaRequest(media))
        }
    }

    private fun turnOn() {
        val ask = VpnService.prepare(this)
        if (ask != null) askVpn.launch(ask) else Net.connect()
    }

    private fun open(url: String) = runCatching { startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url))) }

    private fun copy(ip: String) {
        getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("address", ip))
        Toast.makeText(this, "Copied $ip", Toast.LENGTH_SHORT).show()
    }

    override fun onResume() {
        super.onResume()
        if (Net.state.value.signedIn) Net.refresh()
        Updater.check()
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        if (android.os.Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS) != android.content.pm.PackageManager.PERMISSION_GRANTED
        ) askNotify.launch(android.Manifest.permission.POST_NOTIFICATIONS)
        setContent {
            val dark = isSystemInDarkTheme()
            MaterialTheme(
                colorScheme = if (dark) darkColorScheme(primary = GreenDark, background = Color(0xFF0D1214), surface = Color(0xFF151C1F))
                else lightColorScheme(primary = Green, background = Color(0xFFF4F6F5), surface = Color.White),
            ) {
                Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
                    val s by Net.state.collectAsState()
                    Column(
                        Modifier.safeDrawingPadding().verticalScroll(rememberScrollState()).padding(20.dp),
                        verticalArrangement = Arrangement.spacedBy(16.dp),
                    ) {
                        Text("Cerver", fontSize = 30.sp, fontWeight = FontWeight.ExtraBold)
                        UpdateBar()
                        when {
                            !s.signedIn && s.userCode != null -> Waiting(s)
                            !s.signedIn -> SignedOut(s)
                            else -> { AskCards(s); SignedIn(s) }
                        }
                        s.error?.let { Text(it, color = MaterialTheme.colorScheme.error, fontSize = 14.sp) }
                        UpdateFooter()
                    }
                }
            }
        }
    }

    @Composable
    private fun UpdateBar() {
        val u by Updater.state.collectAsState()
        val latest = u.latest ?: return
        Row(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.primary.copy(alpha = 0.14f)).padding(14.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(Modifier.weight(1f)) {
                Text("Update available: $latest", fontWeight = FontWeight.SemiBold)
                Text(u.progress?.let { "Downloading… $it%" } ?: "You have ${Updater.current}", fontSize = 13.sp)
            }
            Button(onClick = { Updater.update(this@MainActivity) }, enabled = u.progress == null) { Text("Update") }
        }
    }

    @Composable
    private fun UpdateFooter() {
        val u by Updater.state.collectAsState()
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text("Cerver ${Updater.current}", fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f))
            TextButton(onClick = { Updater.check(quiet = false) }) { Text("Check for updates", fontSize = 12.sp) }
        }
        u.message?.let { Text(it, fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f)) }
    }

    @Composable
    private fun SignedOut(s: UiState) {
        Text("Reach your computers from this phone: Screen Sharing, SSH, a dev server, by their Cerver addresses.", fontSize = 17.sp)
        Button(onClick = { Net.signIn(::open) }, enabled = !s.busy, modifier = Modifier.fillMaxWidth()) {
            Text("Sign in", fontSize = 17.sp, modifier = Modifier.padding(6.dp))
        }
    }

    @Composable
    private fun Waiting(s: UiState) {
        Text("Approve this phone in the browser. Check that the code matches:", fontSize = 17.sp)
        Text(s.userCode ?: "", fontFamily = FontFamily.Monospace, fontSize = 34.sp, fontWeight = FontWeight.Bold)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(onClick = { s.verifyUrl?.let(::open) }) { Text("Open the browser again") }
            TextButton(onClick = { Net.cancelSignIn() }) { Text("Cancel") }
        }
    }

    private fun size(b: Long): String = when {
        b >= 1_000_000 -> "%.1f MB".format(b / 1_000_000.0)
        b >= 1_000 -> "${b / 1_000} KB"
        else -> "$b B"
    }

    @Composable
    private fun AskCards(s: UiState) {
        val a by Asks.state.collectAsState()
        val amber = Color(0xFFE0A040)
        a.note?.let { note ->
            Row(Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.surface).padding(14.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(note, fontSize = 15.sp, modifier = Modifier.weight(1f))
                TextButton(onClick = { Asks.clearNote() }) { Text("OK") }
            }
        }
        a.asks.forEach { ask ->
            Column(
                Modifier.fillMaxWidth().clip(RoundedCornerShape(16.dp)).background(amber.copy(alpha = 0.14f)).padding(18.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                if (ask.isSend) {
                    val n = ask.files.size
                    Text("${ask.from} wants to send $n file${if (n == 1) "" else "s"}", fontSize = 20.sp, fontWeight = FontWeight.Bold)
                    Text("\u201C${ask.reason}\u201D", fontSize = 16.sp)
                    ask.files.take(8).forEach { f -> Text("${f.name}  ·  ${size(f.size)}", fontFamily = FontFamily.Monospace, fontSize = 13.sp) }
                    if (n > 8) Text("and ${n - 8} more", fontSize = 13.sp)
                    Text("Saved to Downloads/Cerver, straight from ${ask.from} over your network.", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.65f))
                    if (!s.connected) Text("Turn Cerver on below to receive.", fontSize = 13.sp, color = amber)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Button(onClick = { Net.acceptSend(ask) }, enabled = s.connected && a.sending == null) {
                            Text(if (a.sending == ask.id) "Receiving…" else "Accept")
                        }
                        TextButton(onClick = { Net.denyAsk(ask) }, enabled = a.sending == null) { Text("Decline") }
                    }
                    return@Column
                }
                Text("${ask.from} asks for ${ask.kind}", fontSize = 20.sp, fontWeight = FontWeight.Bold)
                Text("\u201C${ask.reason}\u201D", fontSize = 16.sp)
                Text(
                    "Up to ${ask.max}. Only what you pick is sent, straight to ${ask.from} over your network.",
                    fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.65f),
                )
                if (!s.connected) Text("Turn Cerver on below to send.", fontSize = 13.sp, color = amber)
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Button(onClick = { choose(ask) }, enabled = s.connected && a.sending == null) {
                        Text(if (a.sending == ask.id) "Sending…" else if (ask.kind == "files") "Choose files" else "Choose photos")
                    }
                    TextButton(onClick = { Net.denyAsk(ask) }, enabled = a.sending == null) { Text("Deny") }
                }
            }
        }
    }

    @Composable
    private fun SignedIn(s: UiState) {
        val green = MaterialTheme.colorScheme.primary
        Box(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(16.dp))
                .background(if (s.connected) green.copy(alpha = 0.14f) else MaterialTheme.colorScheme.surface)
                .padding(18.dp),
        ) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(if (s.connected) "Connected" else "Off", fontSize = 24.sp, fontWeight = FontWeight.Bold)
                    Text(
                        if (s.connected) "This phone is ${s.ip ?: "…"} on your network" else "Turn on to reach your computers",
                        fontSize = 15.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.7f),
                    )
                }
                Switch(
                    checked = s.connected,
                    enabled = !s.busy,
                    onCheckedChange = { if (it) turnOn() else Net.disconnect() },
                    colors = SwitchDefaults.colors(checkedTrackColor = green),
                )
            }
        }

        Text("Your network", fontSize = 19.sp, fontWeight = FontWeight.Bold)
        // This phone first, like "you" in the Mac app.
        Row(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.surface).padding(14.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box(Modifier.size(10.dp).clip(CircleShape).background(if (s.connected) green else Color.Gray))
            Spacer(Modifier.size(12.dp))
            Column(Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(s.name, fontSize = 16.sp, fontWeight = FontWeight.SemiBold)
                    Text("  this phone", fontSize = 12.sp, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f))
                }
                Text("Phone · Access Only", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f))
            }
            Text(s.ip ?: "", fontFamily = FontFamily.Monospace, fontSize = 14.sp)
        }
        Text("Computers", fontSize = 13.sp, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f))
        if (s.computers.isEmpty()) {
            Text("None yet. Install Cerver on a computer and run: sudo cerver net up", fontSize = 15.sp)
        }
        s.computers.forEach { c ->
            Row(
                Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.surface)
                    .clickable { copy(if (s.connected) c.host else c.ip) }.padding(14.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Box(Modifier.size(10.dp).clip(CircleShape).background(if (c.connected || (!s.connected && c.online)) green else Color.Gray))
                Spacer(Modifier.size(12.dp))
                Column(Modifier.weight(1f)) {
                    Text(c.name, fontSize = 16.sp, fontWeight = FontWeight.SemiBold)
                    Text(
                        when {
                            s.connected && c.connected -> "connected · ${c.via}"
                            s.connected && c.online -> "connecting…"
                            !c.online -> "offline"
                            else -> "online"
                        },
                        fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f),
                    )
                }
                Text(c.host.ifEmpty { c.ip }, fontFamily = FontFamily.Monospace, fontSize = 14.sp)
            }
        }
        Text(
            "Tap a computer to copy its name. Open it in the browser as http://${s.computers.firstOrNull()?.host ?: "mac-mini"}:3000, or use it in any app, at home or away.",
            fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f),
        )
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            TextButton(onClick = { Net.refresh(); Updater.check(quiet = false) }) { Text("Refresh") }
            TextButton(
                onClick = { Net.signOut() },
                colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
            ) { Text("Remove this phone and sign out") }
        }
    }
}
