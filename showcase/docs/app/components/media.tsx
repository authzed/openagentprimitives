import { useEffect, useState } from 'react'
import { getAsset } from '../manifest'

function Missing({ name }: { name: string }) {
  return <div className="doc-media-missing">missing media: {name}</div>
}

// A full-screen overlay showing an image at its natural size. Click the backdrop
// or press Escape to dismiss; the image itself swallows the click so it stays open.
function Lightbox({ src, caption, onClose }: { src: string; caption?: string; onClose: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose])
  return (
    <div className="doc-lightbox" role="dialog" aria-modal="true" onClick={onClose}>
      <button className="doc-lightbox-close" aria-label="Close" onClick={onClose}>
        ×
      </button>
      <figure className="doc-lightbox-inner" onClick={(e) => e.stopPropagation()}>
        <img src={src} alt={caption ?? ''} />
        {caption && <figcaption>{caption}</figcaption>}
      </figure>
    </div>
  )
}

// An autoplay-muted-loop clip with a poster and click-to-expand. This is how a
// guide embeds a scenario clip captured by the engine.
export function Clip({ name, caption }: { name: string; caption?: string }) {
  const asset = getAsset(name)
  if (!asset || (!asset.webm && !asset.mp4)) return <Missing name={name} />
  return (
    <figure className="doc-clip">
      <video
        className="doc-clip-video"
        controls
        muted
        loop
        playsInline
        preload="metadata"
        poster={asset.poster}
      >
        {asset.webm && <source src={asset.webm} type="video/webm" />}
        {asset.mp4 && <source src={asset.mp4} type="video/mp4" />}
      </video>
      {(caption ?? asset.caption) && <figcaption>{caption ?? asset.caption}</figcaption>}
    </figure>
  )
}

export function Screenshot({ name, caption }: { name: string; caption?: string }) {
  const asset = getAsset(name)
  const src = asset?.src ?? asset?.poster
  const [open, setOpen] = useState(false)
  if (!src) return <Missing name={name} />
  const cap = caption ?? asset?.caption
  return (
    <>
      <figure className="doc-shot">
        <img src={src} alt={cap ?? name} role="button" tabIndex={0} onClick={() => setOpen(true)} />
        {cap && <figcaption>{cap}</figcaption>}
      </figure>
      {open && <Lightbox src={src} caption={cap} onClose={() => setOpen(false)} />}
    </>
  )
}

// A named series fans out to manifest entries name-1, name-2, … laid out as a
// responsive strip.
export function ScreenshotSeries({ name, captions }: { name: string; captions?: string[] }) {
  const shots: { key: string; src: string; caption?: string }[] = []
  for (let i = 1; i < 20; i++) {
    const asset = getAsset(`${name}-${i}`)
    const src = asset?.src ?? asset?.poster
    if (!src) break
    shots.push({ key: `${name}-${i}`, src, caption: captions?.[i - 1] ?? asset?.caption })
  }
  const [open, setOpen] = useState<{ src: string; caption?: string } | null>(null)
  if (shots.length === 0) return <Missing name={`${name}-*`} />
  return (
    <>
      <div className="doc-series">
        {shots.map((s) => (
          <figure className="doc-series-item" key={s.key}>
            <img
              src={s.src}
              alt={s.caption ?? s.key}
              role="button"
              tabIndex={0}
              onClick={() => setOpen({ src: s.src, caption: s.caption })}
            />
            {s.caption && <figcaption>{s.caption}</figcaption>}
          </figure>
        ))}
      </div>
      {open && <Lightbox src={open.src} caption={open.caption} onClose={() => setOpen(null)} />}
    </>
  )
}
