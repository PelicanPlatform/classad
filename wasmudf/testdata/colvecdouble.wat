;; colvecdouble -- the canonical COLUMNAR UDF (ABI version 2): doubles a number.
;;
;; The same function as vecdouble.wat, written against the columnar form, and
;; worth reading beside it. The difference is the whole point of version 2: there
;; is no state byte per element, so there is no branch per element. The state is
;; read ONCE, before the loop, and what remains is a branch-free loop over i64
;; lanes -- which can be unrolled (see colvecsimd.wat), and which retires more
;; than one record per loop back-edge.
;;
;; A columnar descriptor is 16 bytes: form u32 @0 (2), flags u32 @4 (bit 0 const),
;; aPtr u32 @8 pointing at ONE state byte, bPtr u32 @12 pointing at n i64s.
;; States: 0 int, 1 real, 2 bool, 3 undefined, 4 error.
;;
;; The decline mask arrives holding the host's OWN declines -- the records whose
;; value was not of the column's state. This guest neither reads nor clears them:
;; it computes every lane, including those, and the host does not read the lanes
;; it declined. The host puts a benign value in them for exactly that reason.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  ;; declineAll sets every bit of the mask, which escalates the whole batch to the
  ;; scalar entry point. It is how a columnar guest says "this is not a column I
  ;; know how to do" without failing the batch -- a nonzero status would make
  ;; every record ERROR instead of giving each one its scalar answer.
  (func $declineAll (param $decl i32) (param $n i32)
    (local $w i32) (local $words i32)
    (local.set $words (i32.shr_u (i32.add (local.get $n) (i32.const 63)) (i32.const 6)))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $w) (local.get $words)))
      (i64.store (i32.add (local.get $decl) (i32.shl (local.get $w) (i32.const 3)))
                 (i64.const -1))
      (local.set $w (i32.add (local.get $w) (i32.const 1)))
      (br $next))))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $st i32) (local $iPtr i32) (local $rI i32)
    (local $i i32) (local $off i32)

    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    ;; Columnar form, and not const: an all-const batch never reaches a vector
    ;; entry point, so a const flag here would mean the host and this module
    ;; disagree about the ABI.
    (if (i32.ne (i32.load (local.get $args)) (i32.const 2))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=4 (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))

    (local.set $st (i32.load8_u (i32.load offset=8 (local.get $args))))
    (local.set $iPtr (i32.load offset=12 (local.get $args)))
    (local.set $rI (i32.load offset=12 (local.get $res)))

    ;; Integers and reals each get their own loop, chosen once. UNDEFINED, ERROR
    ;; and booleans go to the scalar entry point a record at a time.
    (if (i32.eq (local.get $st) (i32.const 0))
      (then
        (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 0))
        (block $end (loop $next
          (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
          (local.set $off (i32.shl (local.get $i) (i32.const 3)))
          (i64.store (i32.add (local.get $rI) (local.get $off))
            (i64.mul (i64.load (i32.add (local.get $iPtr) (local.get $off)))
                     (i64.const 2)))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $next)))
        (return (i32.const 0))))

    (if (i32.eq (local.get $st) (i32.const 1))
      (then
        (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 1))
        (block $end (loop $next
          (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
          (local.set $off (i32.shl (local.get $i) (i32.const 3)))
          (i64.store (i32.add (local.get $rI) (local.get $off))
            (i64.reinterpret_f64
              (f64.mul (f64.reinterpret_i64 (i64.load (i32.add (local.get $iPtr) (local.get $off))))
                       (f64.const 2))))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $next)))
        (return (i32.const 0))))

    (call $declineAll (local.get $decl) (local.get $n))
    (i32.const 0))

  ;; The scalar entry point: the complete function, and where every declined
  ;; record is served -- the records the host declined because their value was not
  ;; of the column's state, and the whole batch when the state is one this guest
  ;; has no loop for.
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (local $tag i32)
    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    (local.set $tag (i32.load (local.get $a)))
    (i32.store (local.get $res) (local.get $tag))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 0))
    (block $done
      (if (i32.eq (local.get $tag) (i32.const 3)) ;; integer
        (then
          (i64.store offset=8 (local.get $res)
            (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
          (br $done)))
      (if (i32.eq (local.get $tag) (i32.const 4)) ;; real
        (then
          (i64.store offset=8 (local.get $res)
            (i64.reinterpret_f64
              (f64.mul (f64.reinterpret_i64 (i64.load offset=8 (local.get $a)))
                       (f64.const 2))))
          (br $done)))
      ;; undefined and error pass through with the zero payload already written
      (if (i32.le_u (local.get $tag) (i32.const 1))
        (then (br $done)))
      (return (i32.const 2)))
    (i32.const 0))
)
