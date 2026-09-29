// "light" and "dark" name the INK, not the background, so the light-ink file
// is the one for the dark theme. Both render; CSS on the data-theme attribute
// next-themes stamps before paint shows one, so the mark is right on the first
// frame without waiting for React.
import lightInk from "../../docs/assets/brand/oap-wordmark-light.svg";
import darkInk from "../../docs/assets/brand/oap-wordmark-dark.svg";

export function Wordmark({ className = "" }: { className?: string }) {
  return (
    <>
      <img
        className={`${className} wordmark--on-dark`}
        src={lightInk.src}
        alt=""
      />
      <img
        className={`${className} wordmark--on-light`}
        src={darkInk.src}
        alt=""
      />
    </>
  );
}
