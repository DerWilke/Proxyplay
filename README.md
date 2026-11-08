# Proxyplay

Den eigenen Spiele-PC im Browser spielen, ohne Installation und auch in restriktiven Netzwerken, die **WebSockets blockieren** und Antworten über einen Proxy puffern. Solche Netze gibt es zum Beispiel in Hotels, an Unis, in öffentlichen WLANs oder in Unternehmen. Proxyplay läuft nur über ganz normales HTTPS (Long-Polling und POST). Bild: H.264/H.265 über WebCodecs. Ton: Opus.

Gebaut und optimiert für eine Leitung mit ~130–200 ms pro Anfrage hinter einem TLS-Proxy mit HTTP/2. Damit werden auch Spiele wie MMOs oder Strategiespiele gut spielbar.

> **Hinweis:** Nutze das nur in Netzen, in denen du es darfst. Beachte die Regeln des jeweiligen Netzbetreibers.

## Aufbau

```
Browser (unterwegs)  --HTTPS-->  Reverse Proxy (Homeserver)  --HTTP-->  spiel-agent.exe (Spiele-PC)
                                 |- /      -> Webseite (Docker, web/)        |- ffmpeg: Bildschirm + NVENC
                                 |- /api/  -> Agent, mit Login + Schlüssel   |- Ton: WASAPI -> Opus
                                                                             |- Maus/Tastatur: SendInput
```

- **agent/**: Go-Programm für den Windows-Spiele-PC. Es läuft unsichtbar im Hintergrund und trägt sich selbst in den Autostart ein. Der Encoder läuft nur, solange jemand zuschaut.
- **web/**: die Seite (eine einzige `index.html`) und ein kleiner Python-Server, der sie ausliefert und Downloads anbietet.
- **docs/proxy-lessons.md**: was durch einen restriktiven Proxy funktioniert und was nicht. Bitte lesen, bevor man etwas umbaut.

## Voraussetzungen

- Windows-10/11-PC, am besten mit **NVIDIA-Grafikkarte** (NVENC). Ohne NVIDIA rechnet der Prozessor (libx264). Das geht auch, kostet aber Leistung im Spiel.
- Ein Homeserver mit Docker und einem Reverse Proxy mit HTTPS, zum Beispiel Nginx Proxy Manager, Caddy oder Traefik.
- Eine eigene (Sub-)Domain mit gültigem Zertifikat.
- Zum Bauen: [Go](https://go.dev/dl/) auf irgendeinem Rechner (Linux, Mac oder Windows). `go.mod` verlangt 1.27; eine ältere Go-Version lädt die passende beim ersten Bauen automatisch nach.
- `ffmpeg.exe` (getestet: BtbN-Build n8.1 gpl, <https://github.com/BtbN/FFmpeg-Builds/releases>, Datei `ffmpeg-n8.1-latest-win64-gpl-8.1.zip`, daraus `bin/ffmpeg.exe`).

## 1. Agent bauen

```sh
cd agent
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o spiel-agent.exe .
```

Unter Windows (PowerShell): `$env:GOOS="windows"; go build -trimpath -ldflags "-s -w -H windowsgui" -o spiel-agent.exe .`

Tests (Linux/Mac): `go test ./...`

## 2. Agent auf dem Spiele-PC einrichten

1. Ordner anlegen, zum Beispiel `C:\SpielAgent`. Dort hinein: `spiel-agent.exe`, `ffmpeg.exe` und eine Datei `spiel-config.json` (Vorlage: `agent/spiel-config.example.json`):
   ```json
   {
     "server_ip": "192.168.1.10",
     "key": "langer-zufaelliger-schluessel",
     "update_url": "http://192.168.1.10:8104/dl/spiel-agent.exe"
   }
   ```
   - `server_ip`: die LAN-IP des Servers, auf dem der Reverse Proxy läuft. **Nur dieser** (und localhost) darf den Agent ansprechen. Alle anderen Geräte im Netz werden abgewiesen.
   - `key`: ein langer Zufallswert, zum Beispiel aus `openssl rand -hex 24`. Der Reverse Proxy schickt ihn bei jeder Anfrage als Header `X-Spiel-Key` mit.
   - `update_url` ist optional (siehe „Updates“).
2. `spiel-agent.exe` doppelklicken. Bei SmartScreen „Weitere Informationen“ → „Trotzdem ausführen“ wählen. Es öffnet sich kein Fenster.
3. Firewall freigeben, falls Windows nicht von selbst fragt. In einer Admin-PowerShell:
   ```powershell
   New-NetFirewallRule -DisplayName "SpielAgent" -Direction Inbound -Program "C:\SpielAgent\spiel-agent.exe" -Action Allow -Profile Private
   ```
4. Prüfen: Auf dem PC `http://127.0.0.1:8770/api/status` im Browser öffnen. Im Ordner steht ein Protokoll in `spiel-agent.log`.

Der Agent lauscht auf Port 8770 (änderbar mit `"listen": ":8770"`). Die Einstellungen aus der Seite (Auflösung, fps, Bitrate, Codec) speichert er in `spiel-settings.json`.

## 3. Webseite auf dem Homeserver

```sh
cd web
docker compose up -d     # Seite auf Port 8104
```

## 4. Reverse Proxy

Eine Domain, zum Beispiel `spiel.example.de`, mit HTTPS und **Passwortschutz** (Basic Auth) einrichten:

- `/` → `http://<server>:8104` (die Seite)
- `/api/` → `http://<spiele-pc-ip>:8770` (der Agent), mit Header `X-Spiel-Key` und **ohne Pufferung**

Bei Nginx Proxy Manager: einen Proxy-Host für die Seite anlegen (SSL erzwingen, Access List mit Benutzer/Passwort, **„Websockets Support“ aus**). Unter „Advanced“ dann Folgendes eintragen. `<ACCESS-LIST-ID>` ist die Nummer der Access List, also die Datei `/data/access/<id>` im NPM-Container.

```nginx
location /api/ {
    auth_basic            "Authorization required";
    auth_basic_user_file  /data/access/<ACCESS-LIST-ID>;
    proxy_set_header Authorization "";
    proxy_set_header X-Spiel-Key "<DEIN-KEY>";
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_cache off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_connect_timeout 4s;
    client_max_body_size 1m;
    proxy_pass http://<SPIELE-PC-IP>:8770;
}
```

Mit einem anderen Proxy gilt dasselbe Prinzip: Basic Auth, den Header setzen, Pufferung aus, lange Timeouts.

**Wichtig:** Der Agent steuert Maus und Tastatur des PCs. Ohne Passwortschutz und Schlüssel könnte das jeder, der die Adresse kennt.

## 5. Spielen

`https://spiel.example.de` öffnen, einloggen und ins Bild klicken. Der Klick schaltet auch den Ton frei, das verlangen die Browser.

- **Verbindung** (Automatisch / Stream / Abfrage): Automatisch probiert erst einen Stream. Puffert ein Proxy unterwegs, schaltet die Seite auf „Abfrage“ (Long-Polling) um.
- **Bild:** Automatisch (bei Polling = Ausgewogen), Flüssig (am gleichmäßigsten), Direkt (am schnellsten, ruckelt mehr).
- **Schlüsselbilder:** „Selten (neu)“ (Intra-Refresh) vermeidet große Einzelbilder. Das ist auf schwachen Leitungen deutlich besser als „Jede Sekunde (alt)“.
- **Maus glätten:** verteilt ruckelig ankommende Mausbewegungen gleichmäßig. Ausprobieren, was sich besser anfühlt.
- **Codec:** Automatisch nimmt H.265, wenn der Browser es kann, sonst H.264.
- **📊 Messwerte:** Round-Trip, Bildalter, Aussetzer, Ton-Puffer. Die Messung läuft auch bei geschlossenem Panel weiter und ist per `GET /api/reports` abrufbar.
- Erfahrungswert für eine mäßige Leitung: 30 fps, 4 Mbit/s, Schlüsselbilder „Selten (neu)“, Bild „Ausgewogen“ oder „Direkt“.

## Updates (optional)

Legt man eine neue `spiel-agent.exe` in den Download-Ordner der Seite (Docker-Volume `spiel_data`, Unterordner `dl/`), aktualisiert sich der Agent selbst. Dazu vom Server aus aufrufen:

```sh
curl -X POST -H "X-Spiel-Key: <DEIN-KEY>" http://<SPIELE-PC-IP>:8770/api/update
```

Der Agent lädt dann `update_url`, prüft die Datei, tauscht sich aus und startet in unter 2 Sekunden neu.

## Lokal testen ohne Windows

Der Agent läuft auch unter Linux mit einem künstlichen Testbild:

```sh
cd agent && go build -o /tmp/spiel-agent .
SPIEL_LISTEN=127.0.0.1:8771 \
SPIEL_TEST_CMD='ffmpeg -hide_banner -loglevel warning -re -f lavfi -i testsrc2=size={W}x{H}:rate={FPS} -c:v libx264 -preset ultrafast -tune zerolatency -b:v {KBPS}k -g {FPS} -bf 0 -profile:v main -x264-params aud=1:repeat-headers=1 -f h264 pipe:1' \
/tmp/spiel-agent &
cd ../web && API_UPSTREAM=http://127.0.0.1:8771 INDEX_FILE=index.html DATA_DIR=/tmp/spiel-data PORT=8080 python3 server.py
```

Danach `http://127.0.0.1:8080` öffnen.

## Lizenz

MIT, siehe `LICENSE`. `ffmpeg.exe` ist nicht enthalten und steht unter eigener (GPL-)Lizenz.
