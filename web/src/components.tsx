import { useState, type ReactNode } from 'react';
import {
  Button,
  Checkbox,
  Dialog,
  DialogTrigger,
  FieldError,
  Heading,
  Input,
  Label,
  Modal,
  ModalOverlay,
  Text,
  TextField,
  type TextFieldProps,
} from 'react-aria-components';

export function Field({
  label,
  description,
  placeholder,
  min,
  max,
  ...props
}: TextFieldProps & {
  label: string;
  description?: string;
  placeholder?: string;
  min?: number;
  max?: number;
}) {
  return (
    <TextField {...props}>
      <Label>{label}</Label>
      {description && <Text slot="description">{description}</Text>}
      <Input placeholder={placeholder} min={min} max={max} />
      <FieldError />
    </TextField>
  );
}
export function Check({
  children,
  ...props
}: Omit<React.ComponentProps<typeof Checkbox>, 'children'> & {
  children?: ReactNode;
}) {
  return (
    <Checkbox {...props}>
      <span className="check-indicator" aria-hidden="true">
        ✓
      </span>
      {children}
    </Checkbox>
  );
}
export function Confirm({
  label,
  title,
  children,
  onConfirm,
  isDisabled,
  destructive,
}: {
  label: string;
  title: string;
  children: ReactNode;
  onConfirm: () => Promise<void>;
  isDisabled?: boolean;
  destructive?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  return (
    <DialogTrigger
      isOpen={open}
      onOpenChange={(value) => {
        if (busy && !value) return;
        setOpen(value);
        setError('');
      }}
    >
      <Button className={destructive ? 'danger' : undefined} isDisabled={isDisabled}>
        {label}
      </Button>
      <ModalOverlay isDismissable={!busy} isKeyboardDismissDisabled={busy}>
        <Modal>
          <Dialog role="alertdialog">
            <Heading slot="title">{title}</Heading>
            <p>{children}</p>
            {error && (
              <p className="error" role="alert">
                {error}
              </p>
            )}
            <div className="actions">
              <Button className="secondary" isDisabled={busy} onPress={() => setOpen(false)}>
                Cancel
              </Button>
              <Button
                className={destructive ? 'danger' : undefined}
                isDisabled={busy}
                onPress={async () => {
                  setBusy(true);
                  setError('');
                  try {
                    await onConfirm();
                    setOpen(false);
                  } catch (e) {
                    setError(e instanceof Error ? e.message : 'Request failed. Try again.');
                  } finally {
                    setBusy(false);
                  }
                }}
              >
                {busy ? 'Saving…' : 'Confirm'}
              </Button>
            </div>
          </Dialog>
        </Modal>
      </ModalOverlay>
    </DialogTrigger>
  );
}
export function Notice({ message }: { message: string }) {
  return (
    <p className="notice" role="status" aria-live="polite">
      {message}
    </p>
  );
}
export function Status({
  tone = 'idle',
  children,
}: {
  tone?: 'ok' | 'warn' | 'error' | 'idle';
  children: ReactNode;
}) {
  return (
    <p className={`status ${tone}`} role="status">
      <span className="status-dot" aria-hidden="true" />
      {children}
    </p>
  );
}
