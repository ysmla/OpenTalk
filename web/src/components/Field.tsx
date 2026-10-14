import { useId, type ChangeEvent } from 'react';

type FieldProps = {
 label: string;
 value: string;
 onChange: (value: string) => void;
 type?: 'text' | 'email' | 'password';
 hint?: string;
 autoComplete?: string;
 disabled?: boolean;
};

/** Labelled single-line input: every field gets a real <label> and hint wiring. */
export function Field({ label, value, onChange, type = 'text', hint, autoComplete, disabled }: FieldProps) {
 const id = useId();
 const hintId = hint ? `${id}-hint` : undefined;
 return <div className="field">
  <label htmlFor={id}>{label}</label>
  <input
   id={id}
   type={type}
   value={value}
   autoComplete={autoComplete}
   disabled={disabled}
   aria-describedby={hintId}
   onChange={(event: ChangeEvent<HTMLInputElement>) => onChange(event.target.value)}
  />
  {hint && <p className="hint" id={hintId}>{hint}</p>}
 </div>;
}

type TextAreaFieldProps = {
 label: string;
 value: string;
 onChange: (value: string) => void;
 hint?: string;
 disabled?: boolean;
};

export function TextAreaField({ label, value, onChange, hint, disabled }: TextAreaFieldProps) {
 const id = useId();
 const hintId = hint ? `${id}-hint` : undefined;
 return <div className="field">
  <label htmlFor={id}>{label}</label>
  <textarea
   id={id}
   value={value}
   disabled={disabled}
   aria-describedby={hintId}
   onChange={(event: ChangeEvent<HTMLTextAreaElement>) => onChange(event.target.value)}
  />
  {hint && <p className="hint" id={hintId}>{hint}</p>}
 </div>;
}
