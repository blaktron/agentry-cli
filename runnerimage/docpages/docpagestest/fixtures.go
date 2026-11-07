// Package docpagestest makes the documents the Report Designer's tests read:
// a PDF of plain pages and a Word document with a theme, running text and a
// picture.
package docpagestest

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// PDF is a PDF of n A4 pages, each a filled rectangle of fill (an
// "r g b" triple of 0 to 1) across its top and a line of Helvetica.
func PDF(n int, fill string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	kids := make([]string, n)
	for i := range n {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := range n {
		stream := fmt.Sprintf("%s rg 0 700 595 142 re f 0 0 0 rg BT /F1 18 Tf 72 600 Td (Harbour and Vale page %d) Tj ET", fill, i+1)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

// Docx is a Word document with a theme, a font table, a header, a
// footer and one PNG picture.
func Docx(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	add := func(name string, b []byte) {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	add("[Content_Types].xml", []byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`))
	add("word/document.xml", []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Annual Review</w:t></w:r></w:p></w:body></w:document>`))
	add("word/theme/theme1.xml", []byte(`<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="Harbour"><a:themeElements>
<a:clrScheme name="Harbour"><a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1><a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1>
<a:dk2><a:srgbClr val="0B5D4B"/></a:dk2><a:lt2><a:srgbClr val="E7E6E6"/></a:lt2><a:accent1><a:srgbClr val="D97A1E"/></a:accent1><a:accent2><a:srgbClr val="nothex"/></a:accent2></a:clrScheme>
<a:fontScheme name="Harbour"><a:majorFont><a:latin typeface="Fraunces"/><a:ea typeface=""/></a:majorFont><a:minorFont><a:latin typeface="Source Serif 4"/></a:minorFont></a:fontScheme>
</a:themeElements></a:theme>`))
	add("word/fontTable.xml", []byte(`<w:fonts xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:font w:name="Source Serif 4"/><w:font w:name="Inter"/></w:fonts>`))
	add("word/header1.xml", []byte(`<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>Harbour &amp; Vale</w:t></w:r><w:r><w:tab/><w:t>Annual Review</w:t></w:r></w:p></w:hdr>`))
	add("word/footer1.xml", []byte(`<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>Confidential</w:t></w:r></w:p><w:p><w:r><w:t>harbourvale.example</w:t></w:r></w:p></w:ftr>`))
	add("word/media/image1.png", PNG(t, 40, 20, color.NRGBA{11, 93, 75, 255}))
	add("word/media/image2.emf", []byte("not a picture we read"))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// PNG is a w by h PNG of one colour.
func PNG(t testing.TB, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// PicturePDF is a PDF of three A4 pages that draws pictures, uncompressed:
// on page 1 a 120 by 60 logo of fill whose left half is opaque and right
// half transparent (a soft mask), a 300 by 200 opaque photograph of a
// gradient, and a 12 by 12 bullet; on pages 2 and 3 the same logo again,
// as a running header draws it; page 3 draws no new picture.
func PicturePDF() []byte {
	var objs [][]byte
	obj := func(body []byte) int {
		objs = append(objs, body)
		return len(objs)
	}
	stream := func(dict string, data []byte) []byte {
		return append(append([]byte(fmt.Sprintf("%s /Length %d >>\nstream\n", dict, len(data))), data...), []byte("\nendstream")...)
	}
	img := func(w, h int, cs string, data []byte, extra string) []byte {
		return stream(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /%s /BitsPerComponent 8%s", w, h, cs, extra), data)
	}
	obj([]byte("<< /Type /Catalog /Pages 2 0 R >>"))
	obj(nil) // the page tree, written once the pages are known
	var mask, logo, photo, bullet []byte
	for range 60 {
		for x := range 120 {
			a := byte(255)
			if x >= 60 {
				a = 0
			}
			mask = append(mask, a)
			logo = append(logo, 11, 93, 75)
		}
	}
	for y := range 200 {
		for x := range 300 {
			photo = append(photo, byte(x*255/300), byte(y*255/200), 128)
		}
	}
	for range 12 * 12 {
		bullet = append(bullet, 200, 30, 30)
	}
	smask := obj(img(120, 60, "DeviceGray", mask, ""))
	logoObj := obj(img(120, 60, "DeviceRGB", logo, fmt.Sprintf(" /SMask %d 0 R", smask)))
	photoObj := obj(img(300, 200, "DeviceRGB", photo, ""))
	bulletObj := obj(img(12, 12, "DeviceRGB", bullet, ""))
	var kids []string
	for p := 1; p <= 3; p++ {
		xobjects := fmt.Sprintf("/Logo %d 0 R", logoObj)
		draw := "q 120 0 0 60 40 760 cm /Logo Do Q"
		if p == 1 {
			xobjects += fmt.Sprintf(" /Photo %d 0 R /Bullet %d 0 R", photoObj, bulletObj)
			draw += " q 515 0 0 343 40 300 cm /Photo Do Q q 12 0 0 12 40 200 cm /Bullet Do Q"
		}
		contents := obj(stream("<<", []byte(draw)))
		kids = append(kids, fmt.Sprintf("%d 0 R", obj([]byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /XObject << %s >> >> /Contents %d 0 R >>", xobjects, contents)))))
	}
	objs[1] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids)))
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, body := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n", i+1)
		b.Write(body)
		b.WriteString("\nendobj\n")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
