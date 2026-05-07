import { useState } from "react";

interface Props {
  images: string[];
  onStart: (image: string, ttlSeconds: number) => void | Promise<void>;
  busy: boolean;
}

export function ImagePicker({ images, onStart, busy }: Props) {
  // Track only the user's explicit choice; fall back to the first image.
  const [chosen, setChosen] = useState<string | undefined>();
  const [ttl, setTTL] = useState(300);
  const image = chosen ?? images[0] ?? "";

  return (
    <div className="image-picker">
      <h2>Start a session</h2>
      <label>
        Image
        <select
          value={image}
          onChange={(e) => setChosen(e.target.value)}
          disabled={busy}
        >
          {images.map((i) => (
            <option key={i} value={i}>
              {i}
            </option>
          ))}
        </select>
      </label>
      <label>
        TTL (seconds)
        <input
          type="number"
          min={30}
          max={1800}
          value={ttl}
          onChange={(e) => setTTL(Number.parseInt(e.target.value, 10) || 60)}
          disabled={busy}
        />
      </label>
      <button
        type="button"
        onClick={() => onStart(image, ttl)}
        disabled={busy || image === ""}
      >
        {busy ? "Starting..." : "Start session"}
      </button>
    </div>
  );
}
