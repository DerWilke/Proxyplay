//go:build windows

package main

import (
	"fmt"
	"io"
	"runtime"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// captureLoopback records what the default playback device plays (WASAPI loopback)
// and writes raw PCM to w until stop is closed.
func captureLoopback(stop chan struct{}, formatCh chan<- PCMFormat, w io.Writer) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		// already initialised on this thread is fine
		if oe, ok := err.(*ole.OleError); !ok || (oe.Code() != 1 && oe.Code() != 0x80010106) {
			return fmt.Errorf("CoInitializeEx: %v", err)
		}
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return fmt.Errorf("MMDeviceEnumerator: %v", err)
	}
	defer mmde.Release()
	var mmd *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmd); err != nil {
		return fmt.Errorf("kein Wiedergabegerät: %v", err)
	}
	defer mmd.Release()
	var ac *wca.IAudioClient
	if err := mmd.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return fmt.Errorf("IAudioClient: %v", err)
	}
	defer ac.Release()
	var wfx *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&wfx); err != nil {
		return fmt.Errorf("GetMixFormat: %v", err)
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))

	sample := ""
	switch {
	case wfx.WFormatTag == 3 && wfx.WBitsPerSample == 32: // WAVE_FORMAT_IEEE_FLOAT
		sample = "f32le"
	case wfx.WFormatTag == 1 && wfx.WBitsPerSample == 16:
		sample = "s16le"
	case wfx.WFormatTag == 0xFFFE: // WAVE_FORMAT_EXTENSIBLE: SubFormat GUID at offset 24
		sub := *(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(wfx)) + 24))
		if sub == 3 && wfx.WBitsPerSample == 32 {
			sample = "f32le"
		} else if sub == 1 && wfx.WBitsPerSample == 16 {
			sample = "s16le"
		} else if sub == 1 && wfx.WBitsPerSample == 32 {
			sample = "s32le"
		} else if sub == 1 && wfx.WBitsPerSample == 24 {
			sample = "s24le"
		}
	}
	if sample == "" {
		return fmt.Errorf("unbekanntes Tonformat tag=%d bits=%d", wfx.WFormatTag, wfx.WBitsPerSample)
	}
	// 200 ms shared-mode buffer, polled every 10 ms
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, wca.AUDCLNT_STREAMFLAGS_LOOPBACK, 200*10000, 0, wfx, nil); err != nil {
		return fmt.Errorf("Initialize(loopback): %v", err)
	}
	var acc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		return fmt.Errorf("IAudioCaptureClient: %v", err)
	}
	defer acc.Release()
	if err := ac.Start(); err != nil {
		return fmt.Errorf("Start: %v", err)
	}
	defer ac.Stop()
	block := int(wfx.NBlockAlign)
	formatCh <- PCMFormat{Rate: int(wfx.NSamplesPerSec), Channels: int(wfx.NChannels), Sample: sample}

	silence := make([]byte, 0)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
		for {
			var frames uint32
			if err := acc.GetNextPacketSize(&frames); err != nil {
				return fmt.Errorf("GetNextPacketSize: %v", err)
			}
			if frames == 0 {
				break
			}
			var data *byte
			var flags uint32
			var devpos, qpc uint64
			if err := acc.GetBuffer(&data, &frames, &flags, &devpos, &qpc); err != nil {
				return fmt.Errorf("GetBuffer: %v", err)
			}
			n := int(frames) * block
			var chunk []byte
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				if len(silence) < n {
					silence = make([]byte, n)
				}
				chunk = silence[:n]
			} else {
				chunk = make([]byte, n)
				copy(chunk, unsafe.Slice(data, n))
			}
			acc.ReleaseBuffer(frames)
			if _, err := w.Write(chunk); err != nil {
				return nil // encoder gone
			}
		}
	}
}
