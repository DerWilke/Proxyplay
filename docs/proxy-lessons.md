# Echtzeit über normales HTTP hinter einem restriktiven Proxy

Was beim Bau dieses Projekts gemessen und gelernt wurde. Ausgangslage in einem typischen restriktiven Netz (Hotel, Uni, Unternehmen, öffentliches WLAN):
- WebSockets sind komplett blockiert, und auf dem Gerät lässt sich nichts installieren.
- Ein TLS-Proxy bricht HTTPS auf und puffert Antworten.
- Eine Anfrage braucht etwa 130–200 ms, und ab und zu schlägt eine fehl.

## Was so ein Proxy macht
- Gemessen im Testnetz: Round-Trip ~130 ms, runter ~20 Mbit/s, hoch ~11 Mbit/s.
- **Endlose Antworten (chunked) und Server-Sent Events kommen nicht live an.** Der Proxy sammelt die ganze Antwort, bevor er sie weitergibt. Live geht deshalb nur über vollständige, kurze Antworten, also Long-Polling.
- Zum Browser spricht der Proxy **HTTP/2**. Viele parallele Anfragen über eine Verbindung sind darum möglich. In der Seite steht das als „h2“.
- Etwa **1 Anfrage pro Minute** scheitert am Proxy selbst (502/504) und erreicht den eigenen Server gar nicht.
- **Jede Anfrage kostet.** Steigt die Zahl der Anfragen, wird *jede* langsamer:
  - Von ~27 auf ~50 Eingabe-Anfragen pro Sekunde und von 5 auf 8 parallele Abfragen: Die Antwortzeit stieg von ~145 auf 300–490 ms, und der Ton fing an zu stocken.
  - Dabei liefen nur 2 Mbit/s über die Leitung. Bremse war also die Zahl der Anfragen, nicht die Bandbreite.
  - **Regel: Anfragen zusammenfassen, nicht öfter senden.** Ein lokaler Test-Proxy zeigt diesen Effekt nicht.

## Muster, die funktionieren
- **Transport automatisch erkennen:**
  - Erst einen Stream versuchen.
  - Kommen in 4 s weniger als 4 Teile an, puffert jemand. Dann auf Long-Polling umschalten.
  - Einen Schalter zum manuellen Umstellen anbieten.
- **Long-Poll:**
  - `GET ?after=<seq>&gen=<gen>`. Der Server hält die Anfrage bis zu 2 s offen und antwortet, sobald es etwas Neueres gibt.
  - Die Bilder liegen in einem Ringpuffer.
  - Wer zu weit zurückliegt, springt zum neuesten Schlüsselbild.
- **Überlappende Abfragen:**
  - Mit nur einer Abfrage kommt einmal pro Round-Trip ein Schwung Bilder an. Das sieht aus wie 8 fps.
  - Deshalb laufen N Abfragen parallel: 5 bei HTTP/2, 3 bei HTTP/1.1 (dort gibt es nur 6 Verbindungen pro Host).
  - Ein **Cursor pro Client** auf dem Server (Parameter `c`) sorgt dafür, dass zwei Abfragen nie dasselbe Bild bekommen.
  - Das Protokoll liefert `performance.getEntriesByType('resource')[..].nextHopProtocol`.
- **Reihenfolge im Browser:**
  - Parallele Antworten können sich überholen. Bilder deshalb streng nach Nummer weitergeben; ein Schlüsselbild darf vorspringen.
  - Bei einer Lücke nach 60 ms einmal nachfordern (`&re=1`). Nach 600 ms aufgeben und aufs nächste Schlüsselbild warten.
- **Glättung:**
  - Ein Bild wird zu seinem Zeitstempel angezeigt, plus die schnellste gemessene Laufzeit, plus ein Jitter-Polster. Das Polster ist bei „Flüssig“ das p90 und bei „Ausgewogen“ das p60 der letzten ~3 s.
  - „Direkt“ zeigt jedes Bild sofort.
- **Eingaben:**
  - Mehrere nummerierte POSTs `{c, s, e}` sind gleichzeitig unterwegs.
  - Der Server wendet sie streng nach Nummer an und wartet höchstens 150 ms auf eine fehlende. Späte Pakete wendet er trotzdem noch an, damit kein „Taste losgelassen“ verloren geht.
  - Bei Fehlern bis zu 3-mal wiederholen. Der Server ignoriert Doppelte.
- **Eingaben in den Abfragen mitschicken:**
  - Mausbewegungen reisen als POST-Body der nächsten Bildabfrage mit. Nur Tasten und Klicks bekommen eine eigene Anfrage.
  - Das hat beim Umsehen die Anfragen etwa halbiert (~60 → ~30 pro Sekunde).
- **Ton in den Bildabfragen mitschicken:** Statt einer eigenen Ton-Abfrage hängt der Server die Tonpakete an die Bildantworten an. Byte 15 im Kopf markiert ein Tonpaket.
  - Im Browser sitzt ein adaptiver Puffer: Zielwert = p95 der Verspätung + 25 ms, begrenzt auf 60–250 ms.
  - Bei Unterlauf startet der Ton neu. Liegt zu viel im Puffer, werden Pakete verworfen, damit sich keine Verzögerung aufbaut.
- **Fehlertoleranz:**
  - Eine einzelne fehlgeschlagene Anfrage startet nie die Sitzung neu.
  - Neustart erst nach ~8 s ganz ohne Antwort.
  - Die Meldung „nicht erreichbar“ erscheint erst nach 5 s und verschwindet beim nächsten Bild.
  - Ein Zähler „Aussetzer/min“ hilft bei der Fehlersuche.
- **Schlüsselbilder:**
  - Ein volles Schlüsselbild war ~195 KB groß und hat auf schwachen Leitungen jedes Mal einen Ruckler verursacht.
  - Mit NVENC-Intra-Refresh frischt die Karte das Bild streifenweise auf. Größte Einzelbilder ~13 KB, ein volles Bild nur alle 5 s.
  - Das war im Praxistest deutlich besser.

## Stolperfallen
- **ffmpeg mit rohem PCM-Eingang** (`-f s16le/f32le -i pipe:`) liest immer sample_rate/10 Samples am Stück, abgerundet auf eine Zweierpotenz. Bei 44,1 kHz sind das ~93 ms. Der Ton kommt dann in ~100-ms-Schüben, egal wie gut die Leitung ist.
  - Lösung: das PCM in Matroska verpacken (ein Block pro ~10 ms, siehe `agent/mkvpcm.go`) und `-f matroska` verwenden.
  - Lehre: Erst an der Quelle messen, dann der Leitung die Schuld geben.
- **30 fps im Spiel bei 30 fps Aufnahme** erzeugt eine Schwebung (ungleichmäßige Bilder). Das Spiel besser höher laufen lassen als die Aufnahme.
- Zu wenig Bitrate (2 Mbit/s) macht das Bild klotzig, zu viel (6 Mbit/s) verstopft eine schwache Leitung. 4 Mbit/s bei 30 fps war ein guter Kompromiss.

## Leitung lokal nachbauen
Ein kleiner Python-Proxy reicht:
- Er verzögert jede Richtung um +65 ms und puffert Antworten komplett.
- Optional streut er einige Prozent 502-Fehler ein.

Die Seite mit Messcode in headless Firefox laufen lassen. Firefox kann H.264 dekodieren, Playwright-Chromium nicht. Gemessen werden die Streuung des Bildabstands, Lücken >60/250 ms, die zusätzliche Verzögerung und verworfene Bilder.

Gemessen bei 130 ms Leitung und 30 fps:

| Variante | Streuung Bildabstand | Lücken >60 ms pro 8 s | Zusatz-Verzögerung |
|---|---|---|---|
| 1 Abfrage | ~40 ms | 68 | — |
| 5 Abfragen, Direkt | 7 ms | 0 | +60 ms |
| 5 Abfragen, Flüssig | 0,7 ms | 0 | +90 ms |
