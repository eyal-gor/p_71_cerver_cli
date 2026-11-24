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
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
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
                        when {
                            !s.signedIn && s.userCode != null -> Waiting(s)
                            !s.signedIn -> SignedOut(s)
                            else -> SignedIn(s)
                        }
                        s.error?.let { Text(it, color = MaterialTheme.colorScheme.error, fontSize = 14.sp) }
                    }
                }
            }
        }
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

        Text("Your computers", fontSize = 19.sp, fontWeight = FontWeight.Bold)
        if (s.computers.isEmpty()) {
            Text("None yet. Install Cerver on a computer and run: sudo cerver net up", fontSize = 15.sp)
        }
        s.computers.forEach { c ->
            Row(
                Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.surface)
                    .clickable { copy(c.ip) }.padding(14.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Box(Modifier.size(10.dp).clip(CircleShape).background(if (c.online) green else Color.Gray))
                Spacer(Modifier.size(12.dp))
                Column(Modifier.weight(1f)) {
                    Text(c.name, fontSize = 16.sp, fontWeight = FontWeight.SemiBold)
                    Text(
                        when {
                            !c.online -> "offline"
                            !c.reachable -> "online, but no address this phone can use"
                            else -> "online"
                        },
                        fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f),
                    )
                }
                Text(c.ip, fontFamily = FontFamily.Monospace, fontSize = 15.sp)
            }
        }
        Text(
            "Tap a computer to copy its address. Works best on the same Wi-Fi as your computers; away from home it works when your home router lets the connection in.",
            fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f),
        )
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            TextButton(onClick = { Net.refresh() }) { Text("Refresh") }
            TextButton(
                onClick = { Net.signOut() },
                colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
            ) { Text("Remove this phone and sign out") }
        }
        Text("This phone: ${s.name}", fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f))
    }
}
